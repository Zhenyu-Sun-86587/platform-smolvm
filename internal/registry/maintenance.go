package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) SetMaintenance(ctx context.Context, enabled bool, reason string) (Control, error) {
	if enabled && strings.TrimSpace(reason) == "" {
		return Control{}, fmt.Errorf("%w: maintenance reason is required", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Control{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET maintenance = ?, recovery_reason = ? WHERE id = 1", enabled, nullable(reason)); err != nil {
		return Control{}, err
	}
	if err := AppendEvent(ctx, tx, "maintenance", "", "", map[string]any{"maintenance": enabled, "reason": reason}, true); err != nil {
		return Control{}, err
	}
	c, err := readControl(ctx, tx)
	if err != nil {
		return Control{}, err
	}
	return c, tx.Commit()
}

// MarkRestoredHistory prevents automatic dispatch of work recorded in an earlier backup.
func (s *Store) MarkRestoredHistory(ctx context.Context) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if !control.Maintenance {
		return errors.New("restored registry must be in maintenance")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = 'recovery_required', stage = 'backup_restore',
		effect = 'unknown', updated_at = ?, version = version + 1 WHERE control_epoch <> ?
		AND state IN ('queued', 'dispatching', 'observing', 'recovery_required')`,
		time.Now().UnixMilli(), control.ControlEpoch); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runtime_bindings SET observed_state = 'unknown', observed_at = NULL, availability = 'unknown',
			guest_management_status = 'unknown', guest_management_evidence_json = '{}'
		WHERE status <> 'retired';
		UPDATE leases SET state = 'suspect', revision = revision + 1 WHERE state = 'active';
		UPDATE tasks SET state = 'unknown', observed_at = NULL
		WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted');
	`); err != nil {
		return err
	}
	return tx.Commit()
}
