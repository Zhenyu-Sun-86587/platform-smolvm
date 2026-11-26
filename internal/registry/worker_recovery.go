package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"sandbox-platform/internal/placement"
)

type WorkerAuthorityProof struct {
	WorkerID             string            `json:"worker_id"`
	ControlEpoch         string            `json:"control_epoch"`
	EnrollmentGeneration uint64            `json:"enrollment_generation"`
	Session              placement.Session `json:"session"`
	BudgetSequence       int64             `json:"budget_sequence"`
	// Coverage is the authenticated Node reconciliation scope.
	Coverage string `json:"coverage"`
}

type WorkerAuthorityRecovery struct {
	WorkerID           string  `json:"worker_id"`
	ControlEpoch       string  `json:"control_epoch"`
	PreviousGeneration uint64  `json:"previous_generation"`
	ReboundGeneration  *uint64 `json:"rebound_generation,omitempty"`
}

func beginWorkerAuthorityRecovery(ctx context.Context, tx *sql.Tx, epoch string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_authority_recovery
		(worker_id,control_epoch,previous_enrollment_generation)
		SELECT w.id, ?, COALESCE(MAX(e.generation),0) FROM workers w JOIN worker_enrollments e ON e.worker_id=w.id
		WHERE w.desired_mode<>'decommissioned' GROUP BY w.id`, epoch); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_enrollments SET status='revoked', revoked_at=?
		WHERE status='active'`, time.Now().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET observed_status='recovering'
		WHERE id IN (SELECT worker_id FROM worker_authority_recovery WHERE control_epoch=?)`, epoch); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_access_wake_holds SET phase='unknown',covered_sequence=NULL WHERE phase='covered'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase='unknown',covered_sequence=NULL,
		admitted_generation=NULL,admitted_host_boot_id=NULL,admitted_broker_session_id=NULL,admitted_node_session_id=NULL,
		admitted_through_sequence=NULL,admitted_receipt_id=NULL,updated_at=?
		WHERE phase IN ('reserved_not_sent','dispatch_started','unknown','admitted','covered')`, time.Now().UnixMilli()); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListWorkerAuthorityRecovery(ctx context.Context) ([]WorkerAuthorityRecovery, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT worker_id,control_epoch,previous_enrollment_generation,rebound_enrollment_generation
		FROM worker_authority_recovery WHERE control_epoch=(SELECT control_epoch FROM meta WHERE id=1) ORDER BY worker_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerAuthorityRecovery{}
	for rows.Next() {
		var row WorkerAuthorityRecovery
		if err := rows.Scan(&row.WorkerID, &row.ControlEpoch, &row.PreviousGeneration, &row.ReboundGeneration); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ConfirmWorkerAuthority records an authenticated handshake and reconciled
// sample obtained after the Node durably authorized the replacement controller.
func (s *Store) ConfirmWorkerAuthority(ctx context.Context, proof WorkerAuthorityProof) error {
	if proof.WorkerID == "" || proof.EnrollmentGeneration == 0 || !proof.Session.Valid() ||
		!oneOf(proof.Coverage, InventoryKnownOnly, InventoryWorkerFull) || proof.BudgetSequence < 1 {
		return ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if !control.Maintenance || control.ControlEpoch != proof.ControlEpoch {
		return ErrControlEpoch
	}
	var previous uint64
	if err := tx.QueryRowContext(ctx, `SELECT previous_enrollment_generation FROM worker_authority_recovery
		WHERE worker_id=? AND control_epoch=?`, proof.WorkerID, control.ControlEpoch).Scan(&previous); err != nil {
		return notFound(err)
	}
	generation, err := workerGeneration(ctx, tx, proof.WorkerID, control.ControlEpoch)
	if err != nil {
		return err
	}
	if generation <= previous || generation != proof.EnrollmentGeneration {
		return fmt.Errorf("%w: Worker authority must be re-enrolled at a higher generation", ErrConflict)
	}
	sample, err := readWorkerSample(ctx, tx, proof.WorkerID)
	if err != nil {
		return err
	}
	if sample.ControlEpoch != control.ControlEpoch || sample.EnrollmentGeneration != generation ||
		sample.Session != proof.Session || sample.Sequence != proof.BudgetSequence || !sample.Reconciled ||
		time.Since(sample.oldestAt) > placement.DefaultSampleAge {
		return ErrStaleObservation
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_authority_recovery SET rebound_enrollment_generation=?,evidence_json=?
		WHERE worker_id=? AND control_epoch=?`, generation, string(raw), proof.WorkerID, control.ControlEpoch); err != nil {
		return err
	}
	if err := AppendEvent(ctx, tx, "maintenance", "", "",
		map[string]any{"action": "worker_authority_rebound", "worker_id": proof.WorkerID, "generation": generation, "coverage": proof.Coverage}, true); err != nil {
		return err
	}
	return tx.Commit()
}

func checkWorkerAuthorityRecovery(ctx context.Context, tx *sql.Tx) error {
	var unresolved bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_authority_recovery r JOIN meta m ON m.control_epoch=r.control_epoch
		WHERE r.rebound_enrollment_generation IS NULL OR NOT EXISTS (SELECT 1 FROM worker_enrollments e
			WHERE e.worker_id=r.worker_id AND e.generation=r.rebound_enrollment_generation AND e.authority=m.control_epoch AND e.status='active'))`).Scan(&unresolved)
	if err != nil {
		return err
	}
	if unresolved {
		return fmt.Errorf("%w: Worker authority recovery is incomplete", ErrMaintenance)
	}
	return nil
}
