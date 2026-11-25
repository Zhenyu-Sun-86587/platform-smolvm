package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

var ErrGuestQuota = errors.New("Guest grant quota exceeded")

func (s *Service) AuthenticateGuest(ctx context.Context, peer AttestedGuestContext, secret, action, target string) (GuestPrincipal, error) {
	return verifyGuestGrant(ctx, s.store.ReadDB(), peer, secret, action, target)
}

func (s *Service) AuthenticateGuestCredential(ctx context.Context, peer AttestedGuestContext, secret string) (GuestPrincipal, error) {
	return verifyGuestCredential(ctx, s.store.ReadDB(), peer, secret)
}

func (p GuestPrincipal) Limits() (GuestLimits, error) {
	var limits GuestLimits
	err := json.Unmarshal([]byte(p.LimitsJSON), &limits)
	return limits, err
}

func consumeGuestUse(ctx context.Context, tx *sql.Tx, p GuestPrincipal) error {
	updated, err := tx.ExecContext(ctx, `UPDATE guest_grants SET used_count=used_count+1
		WHERE id=? AND used_count<maximum_uses`, p.GrantID)
	if err != nil {
		return err
	}
	if n, err := updated.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrGuestQuota, err)
	}
	return nil
}

func guestConcurrency(ctx context.Context, tx *sql.Tx, p GuestPrincipal) error {
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `DELETE FROM guest_call_leases WHERE expires_at<=?`, now); err != nil {
		return err
	}
	var active int64
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM guest_call_leases l JOIN guest_grants g ON g.id=l.grant_id
		 JOIN guest_identities i ON i.machine_id=g.source_machine_id WHERE i.principal_id=?) +
		(SELECT COUNT(*) FROM guest_operation_authority a JOIN operations o ON o.id=a.operation_id
		 WHERE o.principal_id=? AND o.state IN ('queued','dispatching','observing','recovery_required'))`, p.ID, p.ID).Scan(&active)
	if err != nil {
		return err
	}
	if active >= p.MaximumConcurrent {
		return ErrGuestQuota
	}
	return nil
}

// BeginGuestCall reserves one concurrent authorized request and charges its use.
// Only the grant and lease IDs enter the database.
func (s *Service) BeginGuestCall(ctx context.Context, peer AttestedGuestContext, secret, action, target string,
	ttl time.Duration, requestsPerMinute int, accept func(*sql.Tx, GuestPrincipal) error) (GuestPrincipal, func(), error) {
	var authorized GuestPrincipal
	if ttl <= 0 || ttl > 5*time.Minute || accept == nil {
		return authorized, nil, registry.ErrInvalid
	}
	id, err := registry.NewID("gcall_")
	if err != nil {
		return authorized, nil, err
	}
	err = s.UseGuestGrant(ctx, peer, secret, action, target, func(tx *sql.Tx, guest GuestPrincipal) error {
		if err := guestConcurrency(ctx, tx, guest); err != nil {
			return err
		}
		if requestsPerMinute > 0 {
			window := time.Now().Unix() / 60
			outcome, err := tx.ExecContext(ctx, `INSERT INTO guest_proxy_rate VALUES(?,?,1)
				ON CONFLICT(grant_id) DO UPDATE SET window=excluded.window,
				uses=CASE WHEN window=excluded.window THEN uses+1 ELSE 1 END
				WHERE window!=excluded.window OR uses<?`, guest.GrantID, window, requestsPerMinute)
			if err != nil {
				return err
			}
			if n, err := outcome.RowsAffected(); err != nil || n != 1 {
				return errors.Join(ErrGuestQuota, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO guest_call_leases VALUES(?,?,?)`, id, guest.GrantID,
			time.Now().Add(ttl).UnixMilli()); err != nil {
			return err
		}
		authorized = guest
		return accept(tx, guest)
	})
	if err != nil {
		return GuestPrincipal{}, nil, err
	}
	return authorized, func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = s.store.DB().ExecContext(cleanup, `DELETE FROM guest_call_leases WHERE id=?`, id)
	}, nil
}

func (s *Service) AuthorizeGuestOperation(ctx context.Context, tx *sql.Tx, peer AttestedGuestContext, secret string,
	admitted GuestPrincipal, op registry.Operation, replay bool, memoryMiB int64) error {
	p, err := verifyGuestGrant(ctx, tx, peer, secret, admitted.Action, admitted.Target)
	if err != nil {
		return err
	}
	if p != admitted || op.PrincipalID != p.ID {
		return ErrForbidden
	}
	if replay {
		var grant string
		if err := tx.QueryRowContext(ctx, `SELECT grant_id FROM guest_operation_authority WHERE operation_id=?`, op.ID).Scan(&grant); err != nil || grant != p.GrantID {
			return ErrForbidden
		}
		return nil
	}
	var machineID string
	switch op.Kind {
	case "create":
		if p.Action == GuestCreate {
			machineID = op.MachineID
		}
	case rt.IndependentFork:
		if p.Action == GuestFork && p.Target == op.MachineID && len(op.Children) == 1 {
			machineID = op.Children[0].MachineID
		}
	}
	if machineID == "" || memoryMiB < 1 {
		return ErrForbidden
	}
	if err := guestConcurrency(ctx, tx, p); err != nil {
		return err
	}
	if err := consumeGuestUse(ctx, tx, p); err != nil {
		return err
	}
	limits, err := p.Limits()
	if err != nil {
		return err
	}
	var children, memory int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(c.reserved_memory_mib),0)
		FROM guest_created_machines c JOIN machines m ON m.id=c.machine_id WHERE c.owner_principal_id=?
		AND c.creator_principal_id=? AND m.deleted_at IS NULL`, p.OwnerID, p.ID).Scan(&children, &memory); err != nil {
		return err
	}
	if children >= limits.MaximumChildren || memoryMiB > limits.MaximumMemoryMiB || memory > limits.MaximumMemoryMiB-memoryMiB {
		return ErrGuestQuota
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO guest_created_machines VALUES(?,?,?,?)`, machineID, p.OwnerID, p.ID, memoryMiB); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO guest_operation_authority VALUES(?,?,?,?)`, op.ID, p.CredentialID, p.GrantID, p.Revision); err != nil {
		return err
	}
	return registry.AppendEvent(ctx, tx, "auth", machineID, op.ID, map[string]string{"action": p.Action, "grant_id": p.GrantID}, true)
}
