package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNativeBackupBusy leaves the run available to retry after an already
// dispatched volume deletion has settled.
var ErrNativeBackupBusy = errors.New("a selected volume snapshot is already being removed")

type NativeBackupRoute struct {
	Instance string `json:"runtime_instance_id"`
	Bundle   string `json:"bundle_digest"`
	Reader   string `json:"state_reader_id"`
	Socket   string `json:"worker_socket"`
}

type NativeBackupSelection struct {
	CatalogSHA256   string
	Routes          []NativeBackupRoute
	Snapshots       []string
	VolumeSnapshots []string
}

func (s *Store) NativeBackupSelection(ctx context.Context, id string) (NativeBackupSelection, bool, error) {
	var selection NativeBackupSelection
	var routes []byte
	err := s.reader.QueryRowContext(ctx, `SELECT catalog_sha256,worker_routes_json FROM native_backup_selections WHERE backup_id=?`, id).
		Scan(&selection.CatalogSHA256, &routes)
	if errors.Is(err, sql.ErrNoRows) {
		return selection, false, nil
	}
	if err == nil {
		err = json.Unmarshal(routes, &selection.Routes)
	}
	return selection, err == nil, err
}

// PinNativeBackupSelection seals the copied catalog and its exact logical
// roots together. prepare must persist the staged catalog before returning.
// It issues no Worker RPC while the installation write lock is held.
func (s *Store) PinNativeBackupSelection(ctx context.Context, run BackupRun, prepare func() (NativeBackupSelection, error)) error {
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
	if control.Maintenance {
		return ErrMaintenance
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_runs
		WHERE id=? AND control_epoch=? AND scope='durable_runtime' AND state='running')`, run.ID, run.ControlEpoch).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrOperationState
	}
	selection, err := prepare()
	if err != nil {
		return err
	}
	for _, id := range selection.VolumeSnapshots {
		var deleting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE kind='volume'
			AND state NOT IN ('succeeded','failed','cancelled') AND json_extract(dispatch_json,'$.volume') IS NOT NULL
			AND json_extract(request_json,'$.command.action')='drop_snapshot'
			AND json_extract(request_json,'$.command.snapshot_id')=?)`, id).Scan(&deleting); err != nil {
			return err
		}
		if deleting {
			return ErrNativeBackupBusy
		}
	}
	routes, err := json.Marshal(selection.Routes)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_backup_selections (backup_id,catalog_sha256,worker_routes_json) VALUES(?,?,?)`,
		run.ID, selection.CatalogSHA256, string(routes)); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, id := range selection.Snapshots {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO snapshot_pins(snapshot_id,owner_kind,owner_id,created_at)
			SELECT id,'backup',?,? FROM snapshots WHERE id=? AND state='ready'`, run.ID, now, id); err != nil {
			return err
		}
	}
	for _, id := range selection.VolumeSnapshots {
		if _, err := tx.ExecContext(ctx, `INSERT INTO native_backup_volume_pins VALUES(?,?)`, run.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ConfirmNativeBackupPins(ctx context.Context, id, digest string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE native_backup_selections SET worker_pinned=1 WHERE backup_id=? AND catalog_sha256=?
		AND EXISTS(SELECT 1 FROM backup_runs r JOIN meta m ON m.id=1 WHERE r.id=? AND r.state='running'
			AND r.control_epoch=m.control_epoch AND m.maintenance=0)`, id, digest, id)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return ErrOperationState
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_backup_volume_pins WHERE backup_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_pins WHERE owner_kind='backup' AND owner_id=?
		AND snapshot_id IN (SELECT snapshot_id FROM vm_snapshot_provenance)`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func CheckVolumeBackupPin(ctx context.Context, tx *sql.Tx, snapshot string) error {
	var pinned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_backup_volume_pins WHERE snapshot_id=?)`, snapshot).Scan(&pinned); err != nil {
		return err
	}
	if pinned {
		return ErrNativeBackupBusy
	}
	return nil
}
