package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const LocalWorkerID = rt.LocalWorkerID

type Worker struct {
	ID             string `json:"worker_id"`
	Name           string `json:"name"`
	Endpoint       string `json:"endpoint,omitempty"`
	DesiredMode    string `json:"desired_mode"`
	ObservedStatus string `json:"observed_status"`
	Revision       int64  `json:"revision"`
}

type WorkerEnrollment struct {
	WorkerID               string `json:"worker_id"`
	Generation             int64  `json:"enrollment_generation"`
	CertificateFingerprint string `json:"certificate_fingerprint"`
	Authority              string `json:"authority"`
	Status                 string `json:"status"`
	CreatedAt              int64  `json:"created_at"`
	RevokedAt              *int64 `json:"revoked_at,omitempty"`
}

const workerColumns = `id,name,endpoint,desired_mode,observed_status,revision`

func scanWorker(row scanner) (Worker, error) {
	var worker Worker
	err := row.Scan(&worker.ID, &worker.Name, &worker.Endpoint, &worker.DesiredMode, &worker.ObservedStatus, &worker.Revision)
	return worker, notFound(err)
}

func (s *Store) GetWorker(ctx context.Context, id string) (Worker, error) {
	return scanWorker(s.reader.QueryRowContext(ctx, "SELECT "+workerColumns+" FROM workers WHERE id=?", id))
}

func (s *Store) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT "+workerColumns+" FROM workers ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workers := []Worker{}
	for rows.Next() {
		worker, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	return workers, rows.Err()
}

func validWorkerID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && strings.IndexFunc(id, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_')
	}) < 0
}

// RegisterWorker pins the operator's endpoint configuration. Re-reading an
// unchanged registration preserves maintenance and observation revisions.
func (s *Store) RegisterWorker(ctx context.Context, worker Worker) error {
	if !validWorkerID(worker.ID) || !validateLabel(worker.Name, 255) ||
		(worker.Endpoint != "" && !validateLabel(worker.Endpoint, 2048)) ||
		(worker.ID == LocalWorkerID && worker.Endpoint != "") {
		return fmt.Errorf("%w: invalid Worker registration", ErrInvalid)
	}
	res, err := s.writer.ExecContext(ctx, `INSERT INTO workers(id,name,endpoint) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,endpoint=excluded.endpoint,revision=workers.revision+1
		WHERE workers.desired_mode<>'decommissioned' AND (workers.name<>excluded.name OR workers.endpoint<>excluded.endpoint)`,
		worker.ID, worker.Name, worker.Endpoint)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); n != 0 || err != nil {
		return err
	}
	existing, err := s.GetWorker(ctx, worker.ID)
	if err != nil {
		return err
	}
	if existing.DesiredMode == "decommissioned" {
		return fmt.Errorf("%w: Worker %q is decommissioned", ErrConflict, worker.ID)
	}
	return nil
}

func (s *Store) GetWorkerEnrollment(ctx context.Context, id string) (WorkerEnrollment, error) {
	return readActiveWorkerEnrollment(ctx, s.reader, id)
}

func readActiveWorkerEnrollment(ctx context.Context, db queryer, id string) (WorkerEnrollment, error) {
	var enrollment WorkerEnrollment
	err := db.QueryRowContext(ctx, `SELECT worker_id,generation,certificate_fingerprint,authority,status,created_at
		FROM worker_enrollments WHERE worker_id=? AND status='active'`, id).
		Scan(&enrollment.WorkerID, &enrollment.Generation, &enrollment.CertificateFingerprint, &enrollment.Authority,
			&enrollment.Status, &enrollment.CreatedAt)
	return enrollment, notFound(err)
}

// EnrollWorker records a verified certificate and the control authority that
// approved it. An exact retry is idempotent. Rotation advances the generation
// and retains the old record revoked in the same transaction.
func (s *Store) EnrollWorker(ctx context.Context, enrollment WorkerEnrollment) error {
	if !validWorkerID(enrollment.WorkerID) || enrollment.WorkerID == LocalWorkerID ||
		enrollment.Generation < 1 || !validateLabel(enrollment.CertificateFingerprint, 255) || enrollment.Authority == "" ||
		(enrollment.Status != "" && enrollment.Status != "active") || enrollment.RevokedAt != nil {
		return fmt.Errorf("%w: invalid Worker enrollment", ErrInvalid)
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
	if control.ControlEpoch != enrollment.Authority {
		return ErrControlEpoch
	}
	var desired string
	if err := tx.QueryRowContext(ctx, `SELECT desired_mode FROM workers WHERE id=?`, enrollment.WorkerID).Scan(&desired); err != nil {
		return notFound(err)
	}
	if desired == "decommissioned" {
		return ErrMaintenance
	}
	existing, err := readActiveWorkerEnrollment(ctx, tx, enrollment.WorkerID)
	if err == nil && existing.Generation == enrollment.Generation &&
		existing.CertificateFingerprint == enrollment.CertificateFingerprint && existing.Authority == enrollment.Authority {
		return nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0) FROM worker_enrollments WHERE worker_id=?`, enrollment.WorkerID).
		Scan(&latest); err != nil {
		return err
	}
	if enrollment.Generation != latest+1 {
		var restoring bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_authority_recovery
			WHERE worker_id=? AND control_epoch=? AND rebound_enrollment_generation IS NULL)`,
			enrollment.WorkerID, control.ControlEpoch).Scan(&restoring); err != nil {
			return err
		}
		if !control.Maintenance || !restoring || enrollment.Generation <= latest {
			return fmt.Errorf("%w: Worker enrollment generation must advance", ErrConflict)
		}
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE worker_enrollments SET status='revoked',revoked_at=?
		WHERE worker_id=? AND status='active'`, now, enrollment.WorkerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_enrollments(worker_id,generation,certificate_fingerprint,authority,status,created_at)
		VALUES(?,?,?,?,'active',?)`, enrollment.WorkerID, enrollment.Generation, enrollment.CertificateFingerprint,
		enrollment.Authority, now); err != nil {
		return uniqueConflict(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET observed_status='registering',revision=revision+1 WHERE id=?`, enrollment.WorkerID); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeWorkerEnrollmentTx closes the controller's current authority even when
// unknown local resources make decommission impossible. A later enrollment
// requires an explicitly provisioned higher generation.
func (s *Store) RevokeWorkerEnrollmentTx(ctx context.Context, tx *sql.Tx, worker string, revision, generation int64, reason string) (Worker, error) {
	var state Worker
	if generation < 1 || revision < 1 || !validateLabel(reason, 255) || worker == LocalWorkerID {
		return state, ErrInvalid
	}
	if err := checkWritable(ctx, tx); err != nil {
		return state, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workers SET revision=revision+1,observed_status='unreachable' WHERE id=? AND revision=? AND desired_mode<>'decommissioned'`, worker, revision)
	if err != nil {
		return state, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return state, ErrRevision
	}
	result, err = tx.ExecContext(ctx, `UPDATE worker_enrollments SET status='revoked',revoked_at=? WHERE worker_id=? AND generation=? AND status='active'`, time.Now().UnixMilli(), worker, generation)
	if err != nil {
		return state, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return state, ErrGeneration
	}
	if err := AppendEvent(ctx, tx, "maintenance", "", "", map[string]any{"action": "worker_enrollment_revoked", "worker_id": worker, "generation": generation, "reason": reason}, true); err != nil {
		return state, err
	}
	return scanWorker(tx.QueryRowContext(ctx, "SELECT "+workerColumns+" FROM workers WHERE id=?", worker))
}
