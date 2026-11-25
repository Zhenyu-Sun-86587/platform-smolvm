package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"sandbox-platform/internal/registry"
)

const (
	GuestCreate = "machine.create"
	GuestFork   = "machine.fork"
	GuestExec   = "machine.exec"
	GuestRead   = "machine.read"
)

// AttestedGuestContext is supplied by the host side of the verified transport.
// It cannot be populated by decoding a Guest request body.
type AttestedGuestContext struct {
	MachineID         string `json:"-"`
	BindingID         string `json:"-"`
	RuntimeInstanceID string `json:"-"`
	RuntimeBundleID   string `json:"-"`
	RuntimeObjectID   string `json:"-"`
	IncarnationID     string `json:"-"`
	ExecutionEpoch    int64  `json:"-"`
}

type GuestGrantSpec struct {
	SourceMachineID string    `json:"source_machine_id"`
	Action          string    `json:"action"`
	TargetMachineID string    `json:"target_machine_id,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	MaximumUses     int64     `json:"maximum_uses"`
}

type GuestPrincipal struct {
	ID      string
	GrantID string
	Action  string
	Target  string
}

func guestTarget(action, target string) bool {
	switch action {
	case GuestCreate:
		return target == ""
	case GuestFork, GuestExec, GuestRead:
		return validMachineID(target)
	default:
		return false
	}
}

func requireGrantOwner(ctx context.Context, tx *sql.Tx, owner Principal) error {
	if err := owner.Authorize(ActionAdmin, ""); err != nil {
		return err
	}
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM installation_owner i
		JOIN principals p ON p.id=i.principal_id
		WHERE i.singleton=1 AND p.disabled=0 AND p.id=?
		AND (?='' OR EXISTS(SELECT 1 FROM credentials c
			WHERE c.id=? AND c.principal_id=p.id AND c.kind IN ('api_token','web_session')
			AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>?))))`,
		owner.ID, owner.CredentialID, owner.CredentialID, time.Now().UnixMilli()).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return ErrForbidden
	}
	return nil
}

func (s *Service) CreateGuestGrant(ctx context.Context, owner Principal, grant GuestGrantSpec) (string, error) {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := s.CreateGuestGrantTx(ctx, tx, owner, grant)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Service) CreateGuestGrantTx(ctx context.Context, tx *sql.Tx, owner Principal, grant GuestGrantSpec) (string, error) {
	if !validMachineID(grant.SourceMachineID) || !guestTarget(grant.Action, grant.TargetMachineID) ||
		grant.MaximumUses < 1 || !grant.ExpiresAt.After(time.Now()) {
		return "", registry.ErrInvalid
	}
	if err := requireGrantOwner(ctx, tx, owner); err != nil {
		return "", err
	}
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE id=? AND deleted_at IS NULL`, grant.SourceMachineID).Scan(&source); err != nil {
		return "", err
	}
	var target any
	if grant.TargetMachineID != "" {
		var machine string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE id=? AND deleted_at IS NULL`, grant.TargetMachineID).Scan(&machine); err != nil {
			return "", err
		}
		target = machine
	}
	var principal string
	err := tx.QueryRowContext(ctx, `SELECT principal_id FROM guest_identities WHERE machine_id=?`, source).Scan(&principal)
	if errors.Is(err, sql.ErrNoRows) {
		if principal, err = registry.NewID("gp_"); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO principals(id,username,created_at) VALUES(?,?,?)`,
			principal, principal, time.Now().UnixMilli()); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO guest_identities VALUES(?,?,?)`, source, principal, owner.ID); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	id, err := registry.NewID("gg_")
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO guest_grants
		(id,source_machine_id,action,target_machine_id,expires_at,maximum_uses,revision) VALUES(?,?,?,?,?,?,1)`,
		id, source, grant.Action, target, grant.ExpiresAt.UnixMilli(), grant.MaximumUses); err != nil {
		return "", err
	}
	if err = registry.AppendEvent(ctx, tx, "auth", source, "", map[string]string{"action": "guest_grant_created", "grant_id": id}, true); err != nil {
		return "", err
	}
	return id, nil
}

const currentGuest = `m.id=? AND b.id=? AND b.runtime_instance_id=? AND b.runtime_bundle_id=?
	AND b.runtime_instance_object_id=? AND b.incarnation_id=? AND b.execution_epoch=?
	AND m.deleted_at IS NULL AND b.status='current' AND b.power_state='running'`

func (peer AttestedGuestContext) fields() []any {
	return []any{peer.MachineID, peer.BindingID, peer.RuntimeInstanceID, peer.RuntimeBundleID,
		peer.RuntimeObjectID, peer.IncarnationID, peer.ExecutionEpoch}
}

func (peer AttestedGuestContext) valid() bool {
	return validMachineID(peer.MachineID) && peer.BindingID != "" && peer.RuntimeInstanceID != "" &&
		peer.RuntimeBundleID != "" && peer.RuntimeObjectID != "" && peer.IncarnationID != "" && peer.ExecutionEpoch > 0
}

func (s *Service) IssueGuestCredential(ctx context.Context, owner Principal, peer AttestedGuestContext, grantID string, ttl time.Duration) (string, error) {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	secret, err := s.IssueGuestCredentialTx(ctx, tx, owner, peer, grantID, ttl)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Service) IssueGuestCredentialTx(ctx context.Context, tx *sql.Tx, owner Principal, peer AttestedGuestContext, grantID string, ttl time.Duration) (string, error) {
	if !peer.valid() || grantID == "" || ttl < time.Second || ttl > 10*time.Minute {
		return "", registry.ErrInvalid
	}
	if err := requireGrantOwner(ctx, tx, owner); err != nil {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	secret := "sbg_" + base64.RawURLEncoding.EncodeToString(random[:])
	id, err := registry.NewID("gc_")
	if err != nil {
		return "", err
	}
	now := time.Now()
	fields := append([]any{id, credentialDigest(secret), now.Add(ttl).UnixMilli(), grantID, owner.ID, now.UnixMilli()}, peer.fields()...)
	result, err := tx.ExecContext(ctx, `INSERT INTO guest_credentials
		(id,digest,grant_id,binding_id,runtime_instance_id,runtime_bundle_id,runtime_object_id,incarnation_id,execution_epoch,expires_at)
		SELECT ?,?,g.id,b.id,b.runtime_instance_id,b.runtime_bundle_id,b.runtime_instance_object_id,
			b.incarnation_id,b.execution_epoch,MIN(?,g.expires_at)
		FROM guest_grants g JOIN guest_identities i ON i.machine_id=g.source_machine_id
		JOIN principals p ON p.id=i.principal_id JOIN machines m ON m.id=i.machine_id
		JOIN runtime_bindings b ON b.id=m.current_binding_id AND b.machine_id=m.id
		WHERE g.id=? AND i.owner_principal_id=? AND g.expires_at>? AND g.revoked_at IS NULL
			AND p.disabled=0 AND g.used_count<g.maximum_uses AND `+currentGuest, fields...)
	if err != nil {
		return "", err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return "", errors.Join(ErrForbidden, err)
	}
	if err := registry.AppendEvent(ctx, tx, "auth", peer.MachineID, "", map[string]string{"action": "guest_credential_issued", "grant_id": grantID, "credential_id": id}, true); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Service) RevokeGuestGrant(ctx context.Context, owner Principal, id string, revision int64) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.RevokeGuestGrantTx(ctx, tx, owner, id, revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) RevokeGuestGrantTx(ctx context.Context, tx *sql.Tx, owner Principal, id string, revision int64) error {
	if err := requireGrantOwner(ctx, tx, owner); err != nil {
		return err
	}
	var machine string
	err := tx.QueryRowContext(ctx, `UPDATE guest_grants SET revoked_at=?,revision=revision+1
		WHERE id=? AND revision=? AND revoked_at IS NULL AND source_machine_id IN (
			SELECT machine_id FROM guest_identities WHERE owner_principal_id=?
		) RETURNING source_machine_id`, time.Now().UnixMilli(), id, revision, owner.ID).Scan(&machine)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ErrRevision
	}
	if err != nil {
		return err
	}
	return registry.AppendEvent(ctx, tx, "auth", machine, "", map[string]string{"action": "guest_grant_revoked", "grant_id": id}, true)
}

// UseGuestGrant checks a request against its host identity, current execution
// and exact grant. The callback records the admitted action in the same transaction.
func (s *Service) UseGuestGrant(ctx context.Context, peer AttestedGuestContext, secret, action, target string, accept func(*sql.Tx, GuestPrincipal) error) error {
	if accept == nil {
		return ErrForbidden
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := verifyGuestGrant(ctx, tx, peer, secret, action, target)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE guest_grants SET used_count=used_count+1
		WHERE id=? AND used_count<maximum_uses`, p.GrantID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrForbidden, err)
	}
	if err := accept(tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckGuestGrant rechecks a previously admitted use without acquiring a second use.
func (s *Service) CheckGuestGrant(ctx context.Context, peer AttestedGuestContext, secret string, admitted GuestPrincipal) error {
	current, err := verifyGuestGrant(ctx, s.store.ReadDB(), peer, secret, admitted.Action, admitted.Target)
	if err != nil {
		return err
	}
	if current != admitted {
		return ErrForbidden
	}
	return nil
}

func verifyGuestGrant(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, peer AttestedGuestContext, secret, action, target string) (GuestPrincipal, error) {
	var p GuestPrincipal
	if !peer.valid() || len(secret) != 47 || !strings.HasPrefix(secret, "sbg_") {
		return p, ErrUnauthenticated
	}
	if !guestTarget(action, target) {
		return p, ErrForbidden
	}
	now := time.Now().UnixMilli()
	fields := append([]any{credentialDigest(secret), now, now}, peer.fields()...)
	err := db.QueryRowContext(ctx, `SELECT i.principal_id,g.id,g.action,COALESCE(g.target_machine_id,'')
		FROM guest_credentials c JOIN guest_grants g ON g.id=c.grant_id
		JOIN guest_identities i ON i.machine_id=g.source_machine_id
		JOIN principals p ON p.id=i.principal_id JOIN machines m ON m.id=i.machine_id
		JOIN runtime_bindings b ON b.id=m.current_binding_id AND b.machine_id=m.id
		WHERE c.digest=? AND c.expires_at>? AND g.expires_at>? AND g.revoked_at IS NULL AND p.disabled=0
			AND (g.target_machine_id IS NULL OR EXISTS (SELECT 1 FROM machines target
				WHERE target.id=g.target_machine_id AND target.deleted_at IS NULL))
			AND c.binding_id=b.id AND c.runtime_instance_id=b.runtime_instance_id
			AND c.runtime_bundle_id=b.runtime_bundle_id AND c.runtime_object_id=b.runtime_instance_object_id
			AND c.incarnation_id=b.incarnation_id AND c.execution_epoch=b.execution_epoch AND `+currentGuest,
		fields...).Scan(&p.ID, &p.GrantID, &p.Action, &p.Target)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrUnauthenticated
	}
	if err != nil {
		return p, err
	}
	if p.Action != action || p.Target != target {
		return p, ErrForbidden
	}
	return p, nil
}
