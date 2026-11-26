package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"sandbox-platform/internal/snapshots"
)

func (s *Server) vmSnapshotWorker(ctx context.Context, snapshot snapshots.Snapshot) (workerclient.BindingControl, error) {
	vm := snapshot.VM
	if vm == nil || vm.RuntimeCaptureID == "" || snapshot.Hash == "" {
		return nil, rt.ErrUnsupported
	}
	route, err := s.DB.GetRuntimeRoute(ctx, vm.RuntimeInstanceID)
	if err != nil {
		return nil, err
	}
	if route.BundleID != vm.RuntimeBundleID {
		return nil, rt.ErrUnsupported
	}
	var reader string
	if err = s.DB.ReadDB().QueryRowContext(ctx, `SELECT coalesce(json_extract(manifest_json,'$.contracts.state_reader_id'),'') FROM runtime_bundles WHERE id=?`, route.BundleID).Scan(&reader); err != nil {
		return nil, err
	}
	if reader != vm.StateReaderID || reader == "" {
		return nil, rt.ErrUnsupported
	}
	a, err := s.ForBinding(registry.Binding{WorkerID: route.WorkerID, RuntimeInstanceID: route.InstanceID, RuntimeBundleID: route.BundleID})
	if err != nil {
		return nil, err
	}
	if worker := workerclient.ForAdapter(a, route.BundleID); worker != nil {
		return worker, nil
	}
	return nil, rt.ErrUnsupported
}

func (s *Server) deleteVMSnapshot(ctx context.Context, j *lifecycle.Job) error {
	var request struct {
		ID       string `json:"snapshot_id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(j.Op.Request, &request); err != nil {
		return err
	}
	snapshot, err := s.Snapshots.Get(ctx, request.ID)
	if err != nil {
		return err
	}
	worker, err := s.vmSnapshotWorker(ctx, snapshot)
	if err != nil {
		return err
	}
	var fresh bool
	err = j.StageApply(ctx, "release_vm_snapshot", "", func(ctx context.Context, tx *sql.Tx) error {
		var owns bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM snapshot_pins p JOIN snapshots s ON s.id=p.snapshot_id
			WHERE p.snapshot_id=? AND p.owner_kind='delete' AND p.owner_id=? AND s.state='deleting' AND s.revision=?)`,
			request.ID, j.Op.ID, request.Revision+1).Scan(&owns); err != nil || owns {
			return err
		}
		mark, err := tx.ExecContext(ctx, `UPDATE snapshots SET state='deleting',revision=revision+1
			WHERE id=? AND revision=? AND state='ready'
			AND NOT EXISTS(SELECT 1 FROM snapshot_pins WHERE snapshot_id=?)
			AND NOT EXISTS(SELECT 1 FROM operations WHERE kind IN ('vm_restore','vm_rollback','restore','rollback','snapshot_verify')
				AND state NOT IN ('succeeded','failed','cancelled') AND json_extract(request_json,'$.snapshot_id')=?)`,
			request.ID, request.Revision, request.ID, request.ID)
		if err != nil {
			return err
		}
		if changed, err := mark.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return err
			}
			return &lifecycle.Failure{Code: "DEPENDENCY_BLOCKED", Message: "snapshot is pinned, unavailable or has changed"}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO snapshot_pins VALUES(?,'delete',?,?)`, request.ID, j.Op.ID, time.Now().UnixMilli())
		fresh = err == nil
		return err
	})
	if err != nil {
		var blocked *lifecycle.Failure
		if errors.As(err, &blocked) {
			return j.Failed(ctx, registry.TransitionRequest{Effect: registry.EffectNone,
				Error: lifecycle.JSON(map[string]string{"code": blocked.Code, "message": blocked.Message})})
		}
		return err
	}
	finish := func(state string) func(context.Context, *sql.Tx) error {
		return func(ctx context.Context, tx *sql.Tx) error {
			update, err := tx.ExecContext(ctx, `UPDATE snapshots SET state=?,revision=revision+1
				WHERE id=? AND state='deleting' AND revision=? AND EXISTS(SELECT 1 FROM snapshot_pins
					WHERE snapshot_id=? AND owner_kind='delete' AND owner_id=?)`,
				state, request.ID, request.Revision+1, request.ID, j.Op.ID)
			if err != nil {
				return err
			}
			if changed, err := update.RowsAffected(); err != nil || changed != 1 {
				if err != nil {
					return err
				}
				return registry.ErrRevision
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_pins WHERE snapshot_id=? AND owner_kind='delete' AND owner_id=?`, request.ID, j.Op.ID); err != nil {
				return err
			}
			return registry.AppendEvent(ctx, tx, "snapshot", snapshot.MachineID, j.Op.ID, map[string]string{"snapshot_id": request.ID, "state": state}, false)
		}
	}
	j.Written = true
	if err := worker.ReleaseStatePoint(ctx, snapshot.VM.RuntimeCaptureID, snapshot.Hash, snapshot.VM.OperationID); err != nil {
		var fault *workerclient.Fault
		if errors.As(err, &fault) && (fault.Code == "E_RESOURCE_PINNED" || fresh && fault.Code == "E_CAPABILITY_UNSUPPORTED") {
			code := "DEPENDENCY_BLOCKED"
			if fault.Code == "E_CAPABILITY_UNSUPPORTED" {
				code = "CAPABILITY_UNSUPPORTED"
			}
			return j.Failed(ctx, registry.TransitionRequest{Effect: registry.EffectNone, Apply: finish("ready"),
				Error: lifecycle.JSON(map[string]string{"code": code, "message": fault.Message})})
		}
		return err
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: registry.EffectApplied, Apply: finish("deleted"),
		Result: lifecycle.JSON(map[string]string{"snapshot_id": request.ID})})
}
