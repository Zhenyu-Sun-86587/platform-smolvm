package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type BackupRun struct {
	ID           string     `json:"backup_id"`
	PolicyID     string     `json:"policy_id"`
	ControlEpoch string     `json:"control_epoch"`
	Scope        string     `json:"scope"`
	State        string     `json:"state"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	FailureCode  string     `json:"failure_code,omitempty"`
}

const backupRunColumns = `id,policy_id,control_epoch,scope,state,started_at,finished_at,COALESCE(failure_code,'')`

func scanBackupRun(row scanner) (BackupRun, error) {
	var run BackupRun
	var started int64
	var finished sql.NullInt64
	err := row.Scan(&run.ID, &run.PolicyID, &run.ControlEpoch, &run.Scope, &run.State, &started, &finished, &run.FailureCode)
	run.StartedAt = time.UnixMilli(started).UTC()
	if finished.Valid {
		value := time.UnixMilli(finished.Int64).UTC()
		run.FinishedAt = &value
	}
	return run, err
}

func (s *Store) Backups(ctx context.Context, policy string) ([]BackupRun, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT `+backupRunColumns+` FROM backup_runs WHERE (?='' OR policy_id=?)
		ORDER BY started_at DESC,id DESC`, policy, policy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []BackupRun{}
	for rows.Next() {
		run, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, run)
	}
	return values, rows.Err()
}

func (s *Store) GetBackup(ctx context.Context, id string) (BackupRun, error) {
	run, err := scanBackupRun(s.reader.QueryRowContext(ctx, `SELECT `+backupRunColumns+` FROM backup_runs WHERE id=?`, id))
	return run, notFound(err)
}

func (s *Store) ClaimScheduledBackup(ctx context.Context, policy, scope string, since time.Time) (BackupRun, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BackupRun{}, err
	}
	defer tx.Rollback()
	if err := checkWritable(ctx, tx); errors.Is(err, ErrMaintenance) {
		return BackupRun{}, nil
	} else if err != nil {
		return BackupRun{}, err
	}
	run, err := scanBackupRun(tx.QueryRowContext(ctx, `SELECT `+backupRunColumns+` FROM backup_runs WHERE policy_id=? AND state='running'`, policy))
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BackupRun{}, err
	}
	var recent bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_runs b JOIN meta m ON m.id=1 AND b.control_epoch=m.control_epoch
		WHERE b.policy_id=? AND b.started_at>?)`, policy, since.UnixMilli()).Scan(&recent); err != nil || recent {
		return BackupRun{}, err
	}
	run, err = s.AcceptBackupTx(ctx, tx, policy, scope)
	if err != nil {
		return BackupRun{}, err
	}
	return run, tx.Commit()
}

func (s *Store) AcceptBackupTx(ctx context.Context, tx *sql.Tx, policy, scope string) (BackupRun, error) {
	run := BackupRun{PolicyID: policy, Scope: scope, State: "running", StartedAt: time.Now().UTC().Truncate(time.Millisecond)}
	id, err := NewID("backup_")
	if err != nil {
		return run, err
	}
	run.ID = id
	if err := tx.QueryRowContext(ctx, "SELECT control_epoch FROM meta WHERE id=1").Scan(&run.ControlEpoch); err != nil {
		return run, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO backup_runs (id,policy_id,control_epoch,scope,state,started_at) VALUES(?,?,?,?,?,?)`,
		run.ID, run.PolicyID, run.ControlEpoch, run.Scope, run.State, run.StartedAt.UnixMilli()); err != nil {
		return run, uniqueConflict(err)
	}
	return run, appendBackupEvent(ctx, tx, run)
}

// PinBackupCopy keeps the ready portable snapshot set stable across the DB
// snapshot and artifact export. The reader sees the same committed catalog
// while the writer transaction holds off snapshot deletion.
func (s *Store) PinBackupCopy(ctx context.Context, run BackupRun, copyCatalog func() error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.ControlEpoch != run.ControlEpoch {
		return ErrControlEpoch
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO snapshot_pins (snapshot_id,owner_kind,owner_id,created_at)
		SELECT id,'backup',?,? FROM snapshots WHERE state='ready'`, run.ID, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := copyCatalog(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishBackup(ctx context.Context, run BackupRun, state, code string) error {
	if (run.State != "running" || (state != "verified" && state != "failed")) &&
		(run.State != "verified" || state != "retiring") && (run.State != "retiring" || state != "retired") {
		return ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_pins WHERE owner_kind='backup' AND owner_id=?`, run.ID); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE backup_runs SET state=?,failure_code=?,finished_at=COALESCE(finished_at,?) WHERE id=? AND state=?`,
		state, nullable(code), now.UnixMilli(), run.ID, run.State)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return ErrOperationState
	}
	run.State, run.FailureCode = state, code
	if run.FinishedAt == nil {
		run.FinishedAt = &now
	}
	if err := appendBackupEvent(ctx, tx, run); err != nil {
		return err
	}
	return tx.Commit()
}

func appendBackupEvent(ctx context.Context, tx *sql.Tx, run BackupRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return AppendResourceEvent(ctx, tx, "maintenance", "", "", ResourceEvent{
		Kind: "backup", ID: run.ID, Name: "backup." + run.State, Audience: "admin",
	}, raw, true)
}
