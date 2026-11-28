package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"sandbox-platform/internal/nodeproto"
)

// PinWorkerJournal binds an enrolled Worker to the authenticated journal
// reported by its Node. An established installation upgrading to journal
// fencing needs an offline-confirmed configured pin for its first observation.
func (s *Store) PinWorkerJournal(ctx context.Context, a nodeproto.Authority, node nodeproto.NodeIdentity, configuredID string) error {
	if !a.Valid() || !node.Valid() || configuredID != "" && !nodeproto.ValidJournalID(configuredID) || a.WorkerID == LocalWorkerID {
		return ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode, status string
	err = tx.QueryRowContext(ctx, `SELECT w.desired_mode,w.observed_status FROM workers w
		JOIN worker_enrollments e ON e.worker_id=w.id JOIN meta m ON m.id=1
		WHERE w.id=? AND e.generation=? AND e.status='active'
		AND e.authority=m.control_epoch AND m.control_epoch=? AND m.installation_id=?`,
		a.WorkerID, a.EnrollmentGeneration, a.ControlEpoch, a.ControllerID).Scan(&mode, &status)
	if err != nil {
		return notFound(err)
	}
	if mode == "decommissioned" || status == "identity_conflict" {
		return ErrWorkerIdentityConflict
	}
	var pinned string
	err = tx.QueryRowContext(ctx, `SELECT journal_id FROM worker_node_journals WHERE worker_id=?`, a.WorkerID).Scan(&pinned)
	switch {
	case err == nil && pinned != node.JournalID:
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET observed_status='identity_conflict',revision=revision+1 WHERE id=?`, a.WorkerID); err != nil {
			return err
		}
		if err := AppendEvent(ctx, tx, "maintenance", "", "", map[string]string{
			"action": "worker_journal_identity_conflict", "worker_id": a.WorkerID,
		}, true); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return fmt.Errorf("%w: Worker journal differs from its pinned identity", ErrWorkerIdentityConflict)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if configuredID != "" && configuredID != node.JournalID {
		return fmt.Errorf("%w: Worker journal differs from the configured identity", ErrConflict)
	}
	if pinned == "" {
		var established bool
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM worker_sample_sessions WHERE worker_id=?) OR
			EXISTS(SELECT 1 FROM machines WHERE worker_id=?) OR
			EXISTS(SELECT 1 FROM operations WHERE worker_id=?) OR
			EXISTS(SELECT 1 FROM volumes v JOIN runtime_instances i ON i.id=v.runtime_instance_id WHERE i.worker_id=?)`,
			a.WorkerID, a.WorkerID, a.WorkerID, a.WorkerID).Scan(&established); err != nil {
			return err
		}
		if established && configuredID == "" {
			return fmt.Errorf("%w: established Worker requires an offline-confirmed journal_id", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_node_journals(worker_id,journal_id,observed_node_session) VALUES(?,?,?)`,
			a.WorkerID, node.JournalID, node.NodeSession); err != nil {
			return uniqueConflict(err)
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE worker_node_journals SET observed_node_session=?
		WHERE worker_id=? AND observed_node_session<>?`, node.NodeSession, a.WorkerID, node.NodeSession); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET observed_status='recovering' WHERE id=?
		AND observed_status IN ('ready','degraded')
		AND EXISTS(SELECT 1 FROM worker_samples WHERE worker_id=? AND node_session_id<>?)`,
		a.WorkerID, a.WorkerID, node.NodeSession); err != nil {
		return err
	}
	return tx.Commit()
}

// WorkerNodeIdentity reads the last reconciled identity for ordinary requests,
// or checks that a handshake-selected recovery probe uses the pinned journal.
func (s *Store) WorkerNodeIdentity(ctx context.Context, a nodeproto.Authority, candidate nodeproto.NodeIdentity) (nodeproto.NodeIdentity, error) {
	var identity nodeproto.NodeIdentity
	var offered string
	err := s.reader.QueryRowContext(ctx, `SELECT j.journal_id,j.observed_node_session,COALESCE((SELECT s.node_session_id FROM worker_samples s
		WHERE s.worker_id=w.id AND s.control_epoch=m.control_epoch AND s.enrollment_generation=e.generation),'')
		FROM workers w JOIN worker_node_journals j ON j.worker_id=w.id
		JOIN worker_enrollments e ON e.worker_id=w.id JOIN meta m ON m.id=1
		WHERE w.id=? AND e.generation=? AND e.status='active' AND w.desired_mode<>'decommissioned'
		AND w.observed_status<>'identity_conflict' AND e.authority=m.control_epoch AND m.control_epoch=? AND m.installation_id=?`,
		a.WorkerID, a.EnrollmentGeneration, a.ControlEpoch, a.ControllerID).Scan(&identity.JournalID, &offered, &identity.NodeSession)
	if err != nil {
		return nodeproto.NodeIdentity{}, notFound(err)
	}
	if candidate != (nodeproto.NodeIdentity{}) {
		if !candidate.Valid() || candidate.JournalID != identity.JournalID || candidate.NodeSession != offered {
			return nodeproto.NodeIdentity{}, ErrWorkerIdentityConflict
		}
		return candidate, nil
	}
	if !identity.Valid() || identity.NodeSession != offered {
		return nodeproto.NodeIdentity{}, ErrNotFound
	}
	return identity, nil
}
