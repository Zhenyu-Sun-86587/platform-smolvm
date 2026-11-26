package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/placement"
)

type workerSample struct {
	placement.Sample
	oldestAt       time.Time
	queryStartedAt time.Time
}

func readWorkerSample(ctx context.Context, q queryer, id string) (workerSample, error) {
	var out workerSample
	var raw string
	var oldest, started int64
	err := q.QueryRowContext(ctx, `SELECT report_json, oldest_sample_at, query_started_at
		FROM worker_samples WHERE worker_id = ?`, id).Scan(&raw, &oldest, &started)
	if err != nil {
		return out, notFound(err)
	}
	out.oldestAt = time.UnixMilli(oldest)
	out.queryStartedAt = time.Unix(0, started)
	return out, json.Unmarshal([]byte(raw), &out.Sample)
}

func (s *Store) GetWorkerSample(ctx context.Context, id string) (placement.Sample, time.Time, error) {
	report, err := readWorkerSample(ctx, s.reader, id)
	return report.Sample, report.oldestAt, err
}

func workerGeneration(ctx context.Context, q queryer, worker, epoch string) (uint64, error) {
	var generation uint64
	err := q.QueryRowContext(ctx, `SELECT generation FROM worker_enrollments
		WHERE worker_id=? AND authority=? AND status='active'`, worker, epoch).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) && worker == "local-worker" {
		// Imported single-node installations have no enrollment. A managed
		// local gateway, once enrolled, uses the same rules as any other node.
		var enrolled bool
		err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_enrollments WHERE worker_id=?)`, worker).Scan(&enrolled)
		if err == nil && enrolled {
			err = ErrNotFound
		}
	}
	return generation, notFound(err)
}

// RecordWorkerSample stores one authenticated on-demand resource budget.
// An obsolete session may not replace the latest reconciled one, and a new
// host, broker, or gateway session invalidates prior admission watermarks.
func (s *Store) RecordWorkerSample(ctx context.Context, sample placement.Sample, query placement.Query) error {
	if err := sample.Validate(query, time.Now()); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
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
	generation, err := workerGeneration(ctx, tx, sample.WorkerID, control.ControlEpoch)
	if err != nil {
		return err
	}
	if sample.ControlEpoch != control.ControlEpoch || sample.EnrollmentGeneration != generation {
		return fmt.Errorf("%w: budget authority is stale", ErrConflict)
	}
	var mode, observed string
	if err := tx.QueryRowContext(ctx, `SELECT desired_mode, observed_status FROM workers WHERE id=?`, sample.WorkerID).
		Scan(&mode, &observed); err != nil {
		return notFound(err)
	}
	if mode == "decommissioned" || observed == "identity_conflict" {
		return fmt.Errorf("%w: Worker is outside the current authority", ErrConflict)
	}
	prior, err := readWorkerSample(ctx, tx, sample.WorkerID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	same := err == nil && prior.ControlEpoch == sample.ControlEpoch &&
		prior.EnrollmentGeneration == generation && prior.Session == sample.Session
	if err == nil && !query.StartedAt.After(prior.queryStartedAt) {
		return fmt.Errorf("%w: older Worker budget query", ErrStaleObservation)
	}
	key := []any{sample.WorkerID, sample.ControlEpoch, generation, sample.Session.HostBootID, sample.Session.BrokerSessionID, sample.Session.NodeSessionID}
	if same {
		if sample.Sequence <= prior.Sequence || sample.QueryNonce == prior.QueryNonce {
			return fmt.Errorf("%w: replayed Worker budget", ErrStaleObservation)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_sample_sessions SET latest_sequence=?
			WHERE worker_id=? AND control_epoch=? AND enrollment_generation=? AND host_boot_id=? AND broker_session_id=? AND node_session_id=?`,
			append([]any{sample.Sequence}, key...)...); err != nil {
			return err
		}
	} else {
		if !sample.Reconciled {
			return fmt.Errorf("%w: new Worker session requires reconciliation", ErrStaleObservation)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO worker_sample_sessions VALUES (?,?,?,?,?,?,?)
			ON CONFLICT DO NOTHING`, append(key, sample.Sequence)...)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return fmt.Errorf("%w: retired Worker budget session", ErrStaleObservation)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_access_wake_holds SET phase='unknown',covered_sequence=NULL
			WHERE worker_id=? AND phase='covered'`, sample.WorkerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase='unknown', covered_sequence=NULL,
			admitted_generation=NULL, admitted_host_boot_id=NULL, admitted_broker_session_id=NULL,
			admitted_node_session_id=NULL, admitted_through_sequence=NULL, admitted_receipt_id=NULL, updated_at=?
			WHERE worker_id=? AND phase IN ('dispatch_started','admitted','covered')`, time.Now().UnixMilli(), sample.WorkerID); err != nil {
			return err
		}
	}
	if !sample.Reconciled && !oneOf(observed, "ready", "degraded") {
		return fmt.Errorf("%w: Worker must reconcile before becoming ready", ErrStaleObservation)
	}

	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_samples VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(worker_id) DO UPDATE SET control_epoch=excluded.control_epoch,
		enrollment_generation=excluded.enrollment_generation, host_boot_id=excluded.host_boot_id,
		broker_session_id=excluded.broker_session_id, node_session_id=excluded.node_session_id,
		sequence=excluded.sequence, query_nonce=excluded.query_nonce, query_started_at=excluded.query_started_at,
		received_at=excluded.received_at, oldest_sample_at=excluded.oldest_sample_at, report_json=excluded.report_json`,
		sample.WorkerID, sample.ControlEpoch, generation, sample.Session.HostBootID, sample.Session.BrokerSessionID,
		sample.Session.NodeSessionID, sample.Sequence, sample.QueryNonce, query.StartedAt.UnixNano(),
		query.CompletedAt.UnixMilli(), query.StartedAt.Add(-sample.SampleAge).UnixMilli(), string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET observed_status='ready' WHERE id=?`, sample.WorkerID); err != nil {
		return err
	}
	if err := coverReservations(ctx, tx, sample); err != nil {
		return err
	}
	return tx.Commit()
}

func coverReservations(ctx context.Context, tx *sql.Tx, sample placement.Sample) error {
	_, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase='covered', covered_sequence=?, updated_at=?
		WHERE worker_id=? AND phase='admitted' AND admitted_generation=?
		AND admitted_host_boot_id=? AND admitted_broker_session_id=? AND admitted_node_session_id=?
		AND admitted_through_sequence < ?`, sample.Sequence, time.Now().UnixMilli(), sample.WorkerID,
		sample.EnrollmentGeneration, sample.Session.HostBootID, sample.Session.BrokerSessionID, sample.Session.NodeSessionID, sample.Sequence)
	return err
}
