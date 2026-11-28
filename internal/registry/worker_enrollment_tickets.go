package registry

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

const WorkerEnrollmentTicketLifetime = 5 * time.Minute

type WorkerEnrollmentScope struct {
	WorkerID     string
	Generation   int64
	ControllerID string
	ControlEpoch string
	Endpoint     string
	SPKIPin      string
}

type latestEnrollment struct {
	generation int64
	status     string
	epoch      string
	pin        string
}

func enrollmentTicketGate(ctx context.Context, q queryer, scope WorkerEnrollmentScope) (Control, latestEnrollment, error) {
	var latest latestEnrollment
	control, err := readControl(ctx, q)
	if err != nil {
		return control, latest, err
	}
	if control.ControlEpoch != scope.ControlEpoch || control.InstallationID != scope.ControllerID {
		return control, latest, ErrControlEpoch
	}
	var endpoint, desired, observed string
	err = q.QueryRowContext(ctx, "SELECT endpoint,desired_mode,observed_status FROM workers WHERE id=?", scope.WorkerID).
		Scan(&endpoint, &desired, &observed)
	if err != nil {
		return control, latest, notFound(err)
	}
	if endpoint != scope.Endpoint || desired == "decommissioned" || observed == "identity_conflict" {
		return control, latest, ErrGeneration
	}
	revoked, err := workerEnrollmentKeyRevoked(ctx, q, scope.WorkerID, scope.Generation, scope.SPKIPin)
	if err != nil {
		return control, latest, err
	}
	if revoked {
		return control, latest, ErrGeneration
	}
	err = q.QueryRowContext(ctx, `SELECT generation,status,authority,certificate_fingerprint FROM worker_enrollments
		WHERE worker_id=? ORDER BY generation DESC LIMIT 1`, scope.WorkerID).Scan(&latest.generation, &latest.status, &latest.epoch, &latest.pin)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return control, latest, err
}

func workerEnrollmentKeyRevoked(ctx context.Context, q queryer, worker string, generation int64, pin string) (revoked bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_enrollment_key_revocations
		WHERE worker_id=? AND generation=? AND spki_pin=?)`, worker, generation, pin).Scan(&revoked)
	return
}

func revokeWorkerEnrollmentTicketKey(ctx context.Context, tx *sql.Tx, worker string, generation, now int64, activeRevoked bool) (bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO worker_enrollment_key_revocations(worker_id,generation,spki_pin,revoked_at)
		SELECT t.worker_id,t.generation,t.spki_pin,? FROM worker_enrollment_tickets t JOIN meta m ON m.id=1
		WHERE t.worker_id=? AND t.control_epoch=m.control_epoch AND t.controller_id=m.installation_id
			AND (t.generation=? OR (? AND t.generation>? AND t.certificate IS NOT NULL))`,
		now, worker, generation, activeRevoked, generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) IssueWorkerEnrollmentTicket(ctx context.Context, scope WorkerEnrollmentScope, hash [32]byte, mode string) (time.Time, error) {
	if !validWorkerID(scope.WorkerID) || scope.WorkerID == LocalWorkerID || scope.Generation < 1 ||
		(mode != "initial" && mode != "rotate") {
		return time.Time{}, ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	control, latest, err := enrollmentTicketGate(ctx, tx, scope)
	if err != nil {
		return time.Time{}, err
	}
	if (mode == "initial") != (latest.generation == 0) || scope.Generation <= latest.generation {
		return time.Time{}, ErrGeneration
	}
	if scope.Generation-latest.generation != 1 {
		var recovering bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_authority_recovery
			WHERE worker_id=? AND control_epoch=? AND rebound_enrollment_generation IS NULL)`,
			scope.WorkerID, scope.ControlEpoch).Scan(&recovering)
		if err != nil {
			return time.Time{}, err
		}
		if !control.Maintenance || !recovering {
			return time.Time{}, ErrGeneration
		}
	}
	now := time.Now()
	expires := now.Add(WorkerEnrollmentTicketLifetime).Truncate(time.Millisecond)
	r, err := tx.ExecContext(ctx, `INSERT INTO worker_enrollment_tickets
		(worker_id,generation,controller_id,control_epoch,endpoint,spki_pin,ticket_hash,expires_at,prior_generation,prior_status)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(worker_id) DO UPDATE SET generation=excluded.generation,controller_id=excluded.controller_id,
			control_epoch=excluded.control_epoch,endpoint=excluded.endpoint,spki_pin=excluded.spki_pin,ticket_hash=excluded.ticket_hash,
			expires_at=excluded.expires_at,prior_generation=excluded.prior_generation,prior_status=excluded.prior_status,
			csr_hash=NULL,certificate=NULL
		WHERE worker_enrollment_tickets.certificate IS NULL OR worker_enrollment_tickets.expires_at<=?
			OR worker_enrollment_tickets.generation<>excluded.generation
			OR worker_enrollment_tickets.control_epoch<>excluded.control_epoch
			OR worker_enrollment_tickets.controller_id<>excluded.controller_id
			OR worker_enrollment_tickets.endpoint<>excluded.endpoint OR worker_enrollment_tickets.spki_pin<>excluded.spki_pin`,
		scope.WorkerID, scope.Generation, scope.ControllerID, scope.ControlEpoch, scope.Endpoint, scope.SPKIPin, hash[:],
		expires.UnixMilli(), latest.generation, latest.status, now.UnixMilli())
	if err != nil {
		return time.Time{}, err
	}
	if count, err := r.RowsAffected(); err != nil || count != 1 {
		return time.Time{}, ErrConflict
	}
	return expires, tx.Commit()
}

var ErrWorkerEnrollmentTicket = errors.New("invalid, expired or no longer authorized Worker enrollment ticket")

func lookupWorkerEnrollmentTicket(ctx context.Context, q queryer, scope WorkerEnrollmentScope, hash [32]byte, csr *[32]byte) ([]byte, error) {
	var ticket, priorCSR, certificate []byte
	var expires, prior int64
	var priorStatus string
	err := q.QueryRowContext(ctx, `SELECT ticket_hash,expires_at,prior_generation,prior_status,csr_hash,certificate
		FROM worker_enrollment_tickets WHERE worker_id=? AND generation=? AND controller_id=? AND control_epoch=? AND endpoint=? AND spki_pin=?`,
		scope.WorkerID, scope.Generation, scope.ControllerID, scope.ControlEpoch, scope.Endpoint, scope.SPKIPin).
		Scan(&ticket, &expires, &prior, &priorStatus, &priorCSR, &certificate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkerEnrollmentTicket
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(ticket, hash[:]) != 1 || time.Now().UnixMilli() >= expires ||
		len(certificate) != 0 && csr != nil && subtle.ConstantTimeCompare(priorCSR, csr[:]) != 1 {
		return nil, ErrWorkerEnrollmentTicket
	}
	_, latest, err := enrollmentTicketGate(ctx, q, scope)
	switch {
	case errors.Is(err, ErrControlEpoch), errors.Is(err, ErrGeneration), errors.Is(err, ErrNotFound):
		return nil, ErrWorkerEnrollmentTicket
	case err != nil:
		return nil, err
	}
	samePrevious := latest.generation == prior && latest.status == priorStatus
	activeIssued := len(certificate) != 0 && latest.generation == scope.Generation && latest.status == "active" &&
		latest.epoch == scope.ControlEpoch && latest.pin == scope.SPKIPin
	if !samePrevious && !activeIssued {
		return nil, ErrWorkerEnrollmentTicket
	}
	return certificate, nil
}

func (s *Store) CheckWorkerEnrollmentTicket(ctx context.Context, scope WorkerEnrollmentScope, hash [32]byte) error {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = lookupWorkerEnrollmentTicket(ctx, tx, scope, hash, nil)
	return err
}

func (s *Store) WorkerEnrollmentTicketCertificate(ctx context.Context, scope WorkerEnrollmentScope, hash, csr [32]byte) ([]byte, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return lookupWorkerEnrollmentTicket(ctx, tx, scope, hash, &csr)
}

func (s *Store) ConsumeWorkerEnrollmentTicket(ctx context.Context, scope WorkerEnrollmentScope, hash, csr [32]byte,
	sign func() ([]byte, error)) ([]byte, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	certificate, err := lookupWorkerEnrollmentTicket(ctx, tx, scope, hash, &csr)
	if err != nil || len(certificate) != 0 {
		return certificate, err
	}
	certificate, err = sign()
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE worker_enrollment_tickets SET csr_hash=?,certificate=?
		WHERE worker_id=? AND ticket_hash=? AND certificate IS NULL AND expires_at>?`,
		csr[:], certificate, scope.WorkerID, hash[:], time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return nil, ErrWorkerEnrollmentTicket
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return certificate, nil
}
