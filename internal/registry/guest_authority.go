package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrGuestAuthorityRevoked = errors.New("Guest operation authority is no longer current")

// CheckGuestDispatch revalidates the exact credential recorded at acceptance.
// Owner and internal operations use the existing authorization path.
func (s *Store) CheckGuestDispatch(ctx context.Context, operation string) error {
	var authorized bool
	now := time.Now().UnixMilli()
	err := s.reader.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM guest_credentials c JOIN guest_grants g ON g.id=c.grant_id
		JOIN guest_identities i ON i.machine_id=g.source_machine_id
		JOIN machines m ON m.id=i.machine_id JOIN principals p ON p.id=i.principal_id
		JOIN runtime_bindings b ON b.id=m.current_binding_id AND b.machine_id=m.id
		WHERE a.credential_id=c.id AND a.grant_id=g.id AND a.grant_revision=g.revision
		AND g.revoked_at IS NULL AND g.expires_at>? AND c.expires_at>? AND p.disabled=0
		AND m.deleted_at IS NULL AND b.status='current' AND b.power_state='running'
		AND c.binding_id=b.id AND c.runtime_instance_id=b.runtime_instance_id AND c.runtime_bundle_id=b.runtime_bundle_id
		AND c.runtime_object_id=b.runtime_instance_object_id AND c.incarnation_id=b.incarnation_id
		AND c.execution_epoch=b.execution_epoch
		AND (o.kind='create' AND g.action='machine.create' OR o.kind='independent_fork' AND g.action='machine.fork' AND g.target_machine_id=o.machine_id)
		AND i.principal_id=o.principal_id
	) FROM guest_operation_authority a JOIN operations o ON o.id=a.operation_id WHERE o.id=?`, now, now, operation).Scan(&authorized)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !authorized {
		return ErrGuestAuthorityRevoked
	}
	return nil
}

func (s *Store) GuestOperation(ctx context.Context, operation, principal, grant string) (Operation, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM guest_operation_authority a
		JOIN operations o ON o.id=a.operation_id WHERE o.id=? AND o.principal_id=? AND a.grant_id=?)`,
		operation, principal, grant).Scan(&allowed); err != nil {
		return Operation{}, err
	}
	if !allowed {
		return Operation{}, ErrNotFound
	}
	return readOperation(ctx, tx, operation)
}
