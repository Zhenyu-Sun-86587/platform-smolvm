// Package volumes projects authenticated Worker volume receipts into the API registry.
package volumes

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type Volume struct {
	rt.VolumeRoute
	Name     string `json:"name"`
	WorkerID string `json:"worker_id"`
	rt.VolumeObservation
}

type Attachment struct {
	VolumeID  string `json:"volume_id"`
	MachineID string `json:"machine_id"`
	BindingID string `json:"binding_id"`
	rt.VolumeAttachment
}

type Snapshot struct {
	VolumeID       string `json:"volume_id"`
	VolumeRevision int64  `json:"volume_revision"`
	State          string `json:"state"`
	rt.VolumeSnapshot
}

const selectVolume = `SELECT name,runtime_instance_id,runtime_bundle_id,worker_receipt_json,
	(SELECT i.worker_id FROM runtime_instances i WHERE i.id=volumes.runtime_instance_id) FROM volumes`

type scanner interface{ Scan(...any) error }

func scanVolume(row scanner) (Volume, error) {
	var v Volume
	var receipt string
	if err := row.Scan(&v.Name, &v.RuntimeInstanceID, &v.RuntimeBundleID, &receipt, &v.WorkerID); err != nil {
		return v, err
	}
	return v, json.Unmarshal([]byte(receipt), &v.VolumeObservation)
}

func Get(ctx context.Context, db *sql.DB, owner, id string) (Volume, error) {
	return scanVolume(db.QueryRowContext(ctx, selectVolume+` WHERE owner_id=? AND id=?`, owner, id))
}

func List(ctx context.Context, db *sql.DB, owner string) ([]Volume, error) {
	return readVolumes(db.QueryContext(ctx, selectVolume+` WHERE owner_id=? AND state!='deleted' ORDER BY id`, owner))
}

func OwnedByMachineInstance(ctx context.Context, db *sql.DB, route rt.VolumeRoute, instance string) ([]Volume, error) {
	return readVolumes(db.QueryContext(ctx, selectVolume+`
		WHERE runtime_instance_id=? AND runtime_bundle_id=? AND state!='deleted'
		AND json_extract(worker_receipt_json,'$.machine_instance_id')=? ORDER BY id`,
		route.RuntimeInstanceID, route.RuntimeBundleID, instance))
}

func readVolumes(rows *sql.Rows, err error) ([]Volume, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Volume{}
	for rows.Next() {
		item, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func GetSnapshot(ctx context.Context, db *sql.DB, owner, id string) (Snapshot, error) {
	var v Snapshot
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT s.volume_id,v.revision,s.state,s.pin_receipt_json FROM volume_snapshots s
		JOIN volumes v ON v.id=s.volume_id WHERE v.owner_id=? AND s.id=?`, owner, id).
		Scan(&v.VolumeID, &v.VolumeRevision, &v.State, &raw); err != nil {
		return v, err
	}
	return v, json.Unmarshal([]byte(raw), &v.VolumeSnapshot)
}

func GetAttachment(ctx context.Context, db *sql.DB, owner, id string) (Attachment, error) {
	var v Attachment
	if err := db.QueryRowContext(ctx, `SELECT a.id,a.volume_id,a.machine_id,a.binding_id,a.state,a.revision
		FROM volume_attachments a JOIN volumes v ON v.id=a.volume_id WHERE v.owner_id=? AND a.id=?`, owner, id).
		Scan(&v.ID, &v.VolumeID, &v.MachineID, &v.BindingID, &v.State, &v.Revision); err != nil {
		return v, err
	}
	return v, nil
}

func ListAttachments(ctx context.Context, db *sql.DB, owner, machine string) ([]Attachment, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.id,a.volume_id,a.machine_id,a.binding_id,v.worker_receipt_json
		FROM volume_attachments a JOIN volumes v ON v.id=a.volume_id
		WHERE v.owner_id=? AND a.machine_id=? AND a.state='reserved' ORDER BY a.id`, owner, machine)
	return readAttachments(rows, err)
}

// LaunchDisks supplies the product attachment declarations for the exact Worker
// consumer. The Worker validates the declaration and gives the VMM its real FDs.
func LaunchDisks(ctx context.Context, db *sql.DB, machine, consumer string, route rt.VolumeRoute) ([]rt.LaunchDisk, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.id,a.volume_id,a.machine_id,a.binding_id,v.worker_receipt_json
		FROM volume_attachments a JOIN volumes v ON v.id=a.volume_id
		WHERE a.machine_id=? AND v.runtime_instance_id=? AND v.runtime_bundle_id=? AND a.state='reserved' ORDER BY a.id`,
		machine, route.RuntimeInstanceID, route.RuntimeBundleID)
	attached, err := readAttachments(rows, err)
	if err != nil {
		return nil, err
	}
	devices := make([]rt.LaunchDisk, 0, len(attached))
	for _, lease := range attached {
		if lease.InstanceID != consumer {
			return nil, registry.ErrConflict
		}
		devices = append(devices, rt.LaunchDisk{VolumeID: lease.VolumeID, AttachmentID: lease.ID,
			DeviceID: lease.DeviceKey, Role: "data", ReadOnly: lease.Mode == "ro"})
	}
	return devices, nil
}

func readAttachments(rows *sql.Rows, err error) ([]Attachment, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Attachment{}
	for rows.Next() {
		var item Attachment
		var id, raw string
		if err := rows.Scan(&id, &item.VolumeID, &item.MachineID, &item.BindingID, &raw); err != nil {
			return nil, err
		}
		var observation rt.VolumeObservation
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return nil, err
		}
		for _, lease := range observation.Attachments {
			if lease.ID == id {
				item.VolumeAttachment = lease
				items = append(items, item)
				break
			}
		}
		if item.ID == "" {
			return nil, registry.ErrConflict
		}
	}
	return items, rows.Err()
}

// Apply stores the original Worker graph without recreating ownership of any
// disk generation. It runs with the operation completion transaction.
func Apply(ctx context.Context, tx *sql.Tx, route rt.VolumeRoute, name, machine, binding, attachment, action string, v rt.VolumeObservation) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `INSERT INTO volumes VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,state=excluded.state,worker_receipt_json=excluded.worker_receipt_json
		WHERE volumes.owner_id=excluded.owner_id AND volumes.runtime_bundle_id=excluded.runtime_bundle_id
		AND volumes.runtime_instance_id=excluded.runtime_instance_id AND volumes.revision<=excluded.revision`,
		v.ID, v.OwnerID, name, route.RuntimeInstanceID, route.RuntimeBundleID, v.Revision, v.State, string(raw), time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM volumes
			WHERE id=? AND owner_id=? AND runtime_instance_id=? AND runtime_bundle_id=?`,
			v.ID, v.OwnerID, route.RuntimeInstanceID, route.RuntimeBundleID).Scan(&current); err != nil || current <= v.Revision {
			return registry.ErrRevision
		}
	} else if err := applySnapshots(ctx, tx, v); err != nil {
		return err
	}
	if action == "attach" {
		for _, lease := range v.Attachments {
			if lease.ID == attachment {
				_, err := tx.ExecContext(ctx, `INSERT INTO volume_attachments VALUES(?,?,?,?,'reserved',?)
					ON CONFLICT(id) DO NOTHING`, lease.ID, v.ID, machine, binding, lease.Revision)
				return err
			}
		}
		return registry.ErrConflict
	}
	if action == "detach" {
		_, err := tx.ExecContext(ctx, `UPDATE volume_attachments SET state='detached',revision=revision+1
			WHERE id=? AND volume_id=? AND machine_id=? AND state='reserved'`, attachment, v.ID, machine)
		return err
	}
	return nil
}

func applySnapshots(ctx context.Context, tx *sql.Tx, v rt.VolumeObservation) error {
	if _, err := tx.ExecContext(ctx, `UPDATE volume_snapshots SET state='deleted' WHERE volume_id=?`, v.ID); err != nil {
		return err
	}
	for _, snapshot := range v.Snapshots {
		receipt, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO volume_snapshots VALUES(?,?,?,?,'available',?)
			ON CONFLICT(id) DO UPDATE SET state='available',pin_receipt_json=excluded.pin_receipt_json
			WHERE volume_snapshots.volume_id=excluded.volume_id AND volume_snapshots.generation_handle=excluded.generation_handle
			AND volume_snapshots.source_revision=excluded.source_revision`,
			snapshot.ID, v.ID, snapshot.GenerationID, snapshot.SourceRevision, string(receipt)); err != nil {
			return err
		}
	}
	return nil
}
