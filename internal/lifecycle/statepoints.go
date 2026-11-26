package lifecycle

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"sandbox-platform/internal/snapshots"
	"sandbox-platform/internal/tasks"
	"sandbox-platform/internal/volumes"
)

type statePointRequest struct {
	SnapshotID          string                 `json:"snapshot_id"`
	Label               string                 `json:"label"`
	Purpose             string                 `json:"purpose"`
	VolumeRestorePolicy rt.VolumeRestorePolicy `json:"volume_restore_policy"`
}

func (e *Engine) StatePointCapability(ctx context.Context, b registry.Binding, kind string) (rt.Capability, error) {
	route := bindingRuntime(b)
	if route.Legacy() {
		return rt.FullStateCapabilities(true)[rt.FullStateOperationCapability(kind)], nil
	}
	adapter, err := e.ForBinding(b)
	control, ok := adapter.(rt.StatePointControl)
	if err != nil || !ok {
		return rt.FullStateCapabilities(false)[rt.FullStateOperationCapability(kind)], nil
	}
	return control.StatePointCapability(ctx, kind)
}

func statePointDigest(request rt.StatePointSubmission) string {
	request.RequestDigest = ""
	return fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(request)))
}

func knownActivationDecision(decision string) bool {
	return decision == "" || decision == "none" || decision == "commit"
}

func (j *Job) driveStatePoint(ctx context.Context, routeError error) error {
	control, ok := j.Runtime.(rt.StatePointControl)
	kind := j.Op.Kind
	if kind == "start" {
		kind = rt.VMRestore
	}
	if routeError != nil || !ok {
		if j.Op.State == registry.Queued {
			if err := j.Stage(ctx, "preflight", ""); err != nil {
				return err
			}
		}
		j.Written = j.Intent.StatePoint != nil
		return j.fail(ctx, rt.FullStateUnavailable(kind, bindingRuntime(j.Binding).Legacy()))
	}
	if j.Intent.StatePoint == nil {
		if err := j.prepareDispatch(ctx); err != nil || j.Op.Terminal() {
			return err
		}
		if kind == rt.Hibernate {
			if err := j.checkNativeStandbyActivity(ctx); err != nil {
				return j.fail(ctx, err)
			}
		}
		intent, err := j.prepareStatePoint(ctx, control, kind)
		if errors.Is(err, rt.ErrUnavailable) {
			return err
		}
		if err != nil {
			return j.fail(ctx, err)
		}
		j.Intent.StatePoint = &intent
		if err := j.Stage(ctx, "statepoint_dispatch", ""); err != nil {
			return err
		}
	}
	request := j.Intent.StatePoint
	if request.OperationID != j.Op.ID || request.Kind != kind || request.RequestDigest != statePointDigest(*request) {
		j.Written = true
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "StatePoint dispatch intent changed"})
	}
	receipt, err := control.QueryOperation(ctx, j.Op.ID)
	if launchReceiptMissing(err) {
		receipt, err = control.SubmitStatePoint(ctx, *request)
		if err != nil {
			var capability *rt.CapabilityError
			refusal := statePointRefusal(err)
			if refusal != nil || errors.As(err, &capability) {
				known, queryErr := control.QueryOperation(ctx, j.Op.ID)
				if queryErr == nil {
					return j.confirmStatePoint(ctx, control, known)
				}
				if launchReceiptMissing(queryErr) {
					if refusal == nil {
						refusal = capability
					}
					return j.fail(ctx, refusal)
				}
			}
			return j.awaitStatePoint(ctx, "statepoint_dispatch", "")
		}
	}
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		j.Written = true
		return j.fail(ctx, err)
	}
	return j.confirmStatePoint(ctx, control, receipt)
}

func statePointRefusal(err error) error {
	var fault *rt.Error
	if errors.As(err, &fault) {
		switch fault.NativeCode {
		case "E_VOLUME_GENERATION_MISMATCH", "E_STATEPOINT_CORRUPT", "E_BUNDLE_UNAVAILABLE",
			"E_FENCE_UNPROVEN", "E_RESOURCE_UNAVAILABLE", "E_RESOURCE_LIMIT":
			return &Failure{strings.TrimPrefix(fault.NativeCode, "E_"), fault.Message}
		}
	}
	return nativeRefusal(err)
}

func (j *Job) prepareStatePoint(ctx context.Context, control rt.StatePointControl, kind string) (rt.StatePointSubmission, error) {
	var v statePointRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return rt.StatePointSubmission{}, err
	}
	request := rt.StatePointSubmission{OperationID: j.Op.ID, Kind: kind}
	ref := j.Binding.ExecutionRef()
	request.Expected = rt.LaunchBinding{InstanceID: ref.InstanceID, IncarnationID: &ref.IncarnationID, ExecutionEpoch: ref.ExecutionEpoch}
	if kind == rt.VMRestore {
		before, err := control.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
		if err != nil {
			return request, err
		}
		if before.IncarnationID != nil ||
			(j.Op.Kind == "start" && (before.InstanceID != ref.InstanceID || before.ExecutionEpoch < ref.ExecutionEpoch)) ||
			(j.Op.Kind == rt.VMRestore && before.ExecutionEpoch != 0) {
			return request, &Failure{"STATE_CONFLICT", "StatePoint restore needs the exact stopped Worker slot"}
		}
		request.Expected = before
		request.Parameters.Name = j.Binding.RuntimeName
	} else if ref.InstanceID == "" || ref.IncarnationID == "" || ref.ExecutionEpoch < 1 {
		return request, &Failure{"EXECUTION_EPOCH_UNKNOWN", "StatePoint capture requires a known Worker execution"}
	}
	switch {
	case j.Op.Kind == "start":
		request.Parameters.Mode, request.Parameters.CaptureID = rt.RestoreExact, j.Binding.LastCaptureID
		if j.Binding.PowerState != registry.PowerHibernated || request.Parameters.CaptureID == "" {
			return request, registry.ErrConflict
		}
	case kind == rt.VMRestore || kind == rt.VMRollback:
		request.Parameters.Mode = v.VolumeRestorePolicy
		var instance, bundle, machine, manifest, manifestDigest string
		var bindingID, raw sql.NullString
		err := j.DB.ReadDB().QueryRowContext(ctx, `SELECT v.runtime_capture_id,v.runtime_instance_id,v.runtime_bundle_id,s.source_machine_id,
			o.binding_id,json_extract(o.dispatch_json,'$.source_task_binding'),COALESCE(s.manifest_json,'{}'),COALESCE(s.sha256,'') FROM vm_snapshot_provenance v
			JOIN snapshots s ON s.id=v.snapshot_id LEFT JOIN operations o ON o.id=v.operation_id
			WHERE v.snapshot_id=? AND s.state='ready'`, v.SnapshotID).
			Scan(&request.Parameters.CaptureID, &instance, &bundle, &machine, &bindingID, &raw, &manifest, &manifestDigest)
		if err != nil {
			return request, err
		}
		if instance != j.Binding.RuntimeInstanceID || bundle != j.Binding.RuntimeBundleID || (kind == rt.VMRollback && machine != j.Op.MachineID) {
			return request, &Failure{"BUNDLE_UNAVAILABLE", "StatePoint requires its original Worker slot and Bundle"}
		}
		j.Intent.CaptureConfig = snapshots.CapturedConfiguration(snapshots.Snapshot{
			Hash: manifestDigest, Manifest: json.RawMessage(manifest), VM: &snapshots.VMProvenance{RuntimeCaptureID: request.Parameters.CaptureID},
		})
		if j.Op.Kind == rt.VMRestore && bindingID.Valid {
			if raw.Valid {
				var captured tasks.Binding
				if json.Unmarshal([]byte(raw.String), &captured) == nil && captured.Nonce == bindingID.String && captured.MachineID == machine {
					j.Intent.SourceTaskBinding = &captured
				}
			} else if binding, err := j.DB.GetBinding(ctx, bindingID.String); err == nil && binding.MachineID == machine {
				j.Intent.SourceTaskBinding = GuestBinding(binding)
			}
		}
	default:
		request.Parameters.CaptureID = "point-" + strings.TrimPrefix(j.Op.ID, "op_")
	}
	if kind == rt.VMSnapshot {
		m, err := j.DB.GetMachine(ctx, j.Binding.MachineID)
		if err != nil {
			return request, err
		}
		if m.CurrentBinding == nil || m.CurrentBinding.ID != j.Binding.ID || m.CurrentBinding.ExecutionRef() != ref || m.ActiveOperationID != j.Op.ID {
			return request, registry.ErrGeneration
		}
		config, err := snapshots.CaptureSpec(m.Spec, m.TemplateID, m.Revision, request.Parameters.CaptureID)
		if err != nil {
			return request, err
		}
		j.Intent.SourceTaskBinding, j.Intent.CaptureConfig = GuestBinding(j.Binding), config
	}
	request.RequestDigest = statePointDigest(request)
	return request, nil
}

func (j *Job) awaitStatePoint(ctx context.Context, stage string, disposition registry.SourceDisposition) error {
	if j.Op.State == registry.Observing && j.Op.Stage == stage && j.Op.SourceDisposition == disposition {
		return nil
	}
	return j.transition(ctx, registry.TransitionRequest{State: registry.Observing, Stage: stage, Effect: registry.EffectUnknown, SourceDisposition: disposition})
}

func (j *Job) confirmStatePoint(ctx context.Context, control rt.StatePointControl, receipt rt.OperationReceipt) error {
	request := j.Intent.StatePoint
	kind := request.Kind
	if kind == rt.VMRestore {
		kind = "restore_target"
	}
	point := receipt.StatePoint
	j.Written = true
	if receipt.OperationID != request.OperationID || receipt.Kind != kind || receipt.RequestDigest != request.RequestDigest ||
		receipt.BundleDigest != j.Binding.RuntimeBundleID || receipt.RouteID != "deep" || receipt.InstanceID != request.Expected.InstanceID ||
		receipt.ExpectedExecutionEpoch != request.Expected.ExecutionEpoch || !sameIncarnation(receipt.ExpectedIncarnationID, request.Expected.IncarnationID) ||
		point == nil || point.CaptureID != request.Parameters.CaptureID {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker receipt differs from the persisted StatePoint dispatch scope"})
	}
	if !knownActivationDecision(point.ActivationDecision) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker StatePoint activation decision is unrecognized"})
	}
	if receipt.Phase == "NEW" || (request.Kind == rt.VMRollback && receipt.Phase == "COMPLETED" && point.TargetOperationID == "") {
		stage := point.Stage
		if stage == "" {
			stage = "statepoint_pending"
		}
		return j.awaitStatePoint(ctx, stage, "")
	}
	if receipt.Phase == "ABORTED" {
		if request.Kind == rt.VMRollback && point.Phase == "hibernated" {
			return j.completeStatePointStop(ctx, control, *point, true)
		}
		code := point.ErrorCode
		if code == "" {
			code = "NATIVE_OPERATION_ABORTED"
		}
		step := registry.TransitionRequest{Effect: registry.EffectNone,
			Error: JSON(map[string]string{"code": strings.TrimPrefix(code, "E_"), "message": point.Error})}
		if point.ActivationDecision == "commit" {
			if kind != "restore_target" || receipt.Decision != "ABORT" || point.Stage != "aborted" || receipt.Execution != nil {
				return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker has no fenced disposition for the committed StatePoint target"})
			}
			step.Effect, step.FencedActivationFailure = registry.EffectUnknown, true
		}
		return j.Failed(ctx, step)
	}
	if receipt.Phase != "COMPLETED" || !statePointDigestValid(point.ManifestDigest) || point.RepresentationID == "" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker did not prove the committed StatePoint root"})
	}
	switch request.Kind {
	case rt.VMSnapshot:
		if !point.SourceReleased || point.Phase != "durable" || (point.OriginalSourceDisposition != "RUNNING" && point.OriginalSourceDisposition != "STANDBY") {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "checkpoint lacks the returned source and durable root"})
		}
		return j.completeCheckpoint(ctx, *point)
	case rt.Hibernate:
		return j.completeStatePointStop(ctx, control, *point, false)
	case rt.VMRollback:
		target, err := control.QueryOperation(ctx, point.TargetOperationID)
		if errors.Is(err, rt.ErrUnavailable) {
			return err
		}
		if err != nil {
			return j.fail(ctx, err)
		}
		if target.Kind != "restore_target" || target.InstanceID != receipt.InstanceID || target.BundleDigest != receipt.BundleDigest ||
			target.ExpectedExecutionEpoch != receipt.ExpectedExecutionEpoch || target.StatePoint == nil ||
			target.StatePoint.CaptureID != point.CaptureID || target.StatePoint.RescueCaptureID != point.RescueCaptureID ||
			!knownActivationDecision(target.StatePoint.ActivationDecision) {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "rollback target differs from its saved rescue capture"})
		}
		switch target.Phase {
		case "NEW":
			return j.awaitStatePoint(ctx, target.StatePoint.Stage, registry.SourceExited)
		case "ABORTED":
			point.Error = target.StatePoint.Error
			return j.completeStatePointStop(ctx, control, *point, true)
		case "COMPLETED":
			return j.completeStatePointTarget(ctx, control, target, point)
		default:
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "rollback target has no verified final disposition"})
		}
	default:
		return j.completeStatePointTarget(ctx, control, receipt, nil)
	}
}

func sameIncarnation(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func statePointDigestValid(value string) bool {
	digest, present := strings.CutPrefix(value, "sha256:")
	_, err := hex.DecodeString(digest)
	return present && len(digest) == 64 && err == nil
}

func (j *Job) recordStatePoint(ctx context.Context, tx *sql.Tx, p rt.StatePointProgress, capture, purpose string) error {
	digest := p.ManifestDigest
	if purpose == "rollback_rescue" && p.RescueCaptureID == capture && p.RescueManifestDigest != "" {
		if !statePointDigestValid(p.RescueManifestDigest) {
			return registry.ErrConflict
		}
		digest = p.RescueManifestDigest
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO managed_statepoint_receipts
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(runtime_instance_id,capture_id) DO NOTHING`,
		j.Binding.RuntimeInstanceID, capture, p.RepresentationID, j.Binding.RuntimeBundleID, j.Binding.ID,
		j.Op.ID, purpose, digest, string(JSON(p)))
	if err != nil {
		return err
	}
	return registry.AppendResourceEvent(ctx, tx, "snapshot", j.Binding.MachineID, j.Op.ID, registry.ResourceEvent{
		Kind: "statepoint", ID: capture, Name: "statepoint.ready", Audience: "read",
	}, JSON(map[string]string{"capture_id": capture, "purpose": purpose}), false)
}

func (j *Job) completeCheckpoint(ctx context.Context, point rt.StatePointProgress) error {
	var v statePointRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return j.fail(ctx, err)
	}
	id := "snap_" + strings.TrimPrefix(j.Op.ID, "op_")
	if v.Purpose == "" {
		v.Purpose = "recovery"
	}
	taskStatus := "unavailable"
	if TaskMetadataStatus(j.Binding) != "unavailable" {
		taskStatus = "stale"
	}
	config := j.Intent.CaptureConfig
	if config != nil && config.CaptureID == point.CaptureID {
		fixed := *config
		fixed.ManifestDigest = point.ManifestDigest
		config = &fixed
	}
	manifest := struct {
		rt.StatePointProgress
		TaskMetadataStatus string                          `json:"task_metadata_status"`
		Configuration      *snapshots.CaptureConfiguration `json:"captured_configuration,omitempty"`
		Scope              *snapshots.CapturedScope        `json:"captured_scope,omitempty"`
	}{point, taskStatus, config, j.observeCapturedScope(ctx, point)}
	return j.Done(ctx, registry.TransitionRequest{Effect: registry.EffectApplied, SourceDisposition: registry.SourceDisposition(point.OriginalSourceDisposition),
		Result: JSON(map[string]any{"snapshot_id": id, "capture_id": point.CaptureID, "statepoint": point, "task_metadata_status": taskStatus}),
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if err := j.recordStatePoint(ctx, tx, point, point.CaptureID, "checkpoint"); err != nil {
				return err
			}
			now := time.Now().UnixMilli()
			if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,label,task_content,
				state,relative_path,sha256,manifest_json,provenance,closure_verified_at,created_at)
				VALUES(?,?,?,?,?,'unknown','ready',?,?,?,'worker',?,?)`,
				id, j.Binding.MachineID, j.Binding.Generation, v.Purpose, v.Label, id, point.ManifestDigest, string(JSON(manifest)), now, now); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO vm_snapshot_provenance
				SELECT ?,?,?,id,json_extract(manifest_json,'$.contracts.state_reader_id'),? FROM runtime_bundles WHERE id=?`,
				id, j.Op.ID, j.Binding.RuntimeInstanceID, point.CaptureID, j.Binding.RuntimeBundleID)
			if err != nil {
				return err
			}
			return registry.AppendEvent(ctx, tx, "snapshot", j.Binding.MachineID, j.Op.ID, map[string]string{"snapshot_id": id, "state": "ready"}, false)
		}})
}

// The durable receipt authorizes publication even if this optional detailed
// readback is unavailable (for example with a historical Worker).
func (j *Job) observeCapturedScope(ctx context.Context, point rt.StatePointProgress) *snapshots.CapturedScope {
	client := workerclient.ForAdapter(j.Runtime, j.Binding.RuntimeBundleID)
	request := j.Intent.StatePoint
	if client == nil || request == nil || request.Expected.IncarnationID == nil {
		return nil
	}
	ref := workerclient.Execution{InstanceID: request.Expected.InstanceID, IncarnationID: *request.Expected.IncarnationID,
		Epoch: request.Expected.ExecutionEpoch}
	raw, err := client.StatePointCapture(ctx, ref, request.OperationID, request.RequestDigest, point.CaptureID)
	if err != nil {
		return nil
	}
	return snapshots.ReadCapturedScope(raw, *request, point)
}

func (j *Job) completeStatePointStop(ctx context.Context, control rt.StatePointControl, point rt.StatePointProgress, rollback bool) error {
	binding, err := control.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	ref := j.Binding.ExecutionRef()
	capture, purpose := point.CaptureID, "hibernate"
	if rollback {
		capture, purpose = point.RescueCaptureID, "rollback_rescue"
	}
	if point.Phase != "hibernated" || !point.SourceReleased || binding.InstanceID != ref.InstanceID ||
		binding.IncarnationID != nil || binding.ExecutionEpoch < ref.ExecutionEpoch ||
		!statePointDigestValid(point.ManifestDigest) || point.RepresentationID == "" || capture == "" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "durable capture has no proved stopped source"})
	}
	observation := nativeObservation(j.Binding, ref, registry.PowerHibernated, "")
	observation.Native.HibernatedCapture = capture
	observation.RuntimeIdentity = nil
	transition := registry.TransitionRequest{Observation: &observation, Effect: registry.EffectApplied,
		DesiredPower: "hibernated", SourceDisposition: registry.SourceExited,
		Result: JSON(map[string]any{"capture_id": capture, "statepoint": point}),
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			return j.recordStatePoint(ctx, tx, point, capture, purpose)
		},
	}
	if rollback {
		transition.Effect = registry.EffectPartial
		transition.Error = JSON(map[string]string{"code": "ROLLBACK_TARGET_ABORTED", "message": point.Error})
		return j.finish(ctx, registry.Failed, transition)
	}
	return j.Done(ctx, transition)
}

func (j *Job) completeStatePointTarget(ctx context.Context, control rt.StatePointControl, receipt rt.OperationReceipt, rescue *rt.StatePointProgress) error {
	point, ref := receipt.StatePoint, receipt.Execution
	if point == nil || ref == nil || ref.InstanceID != receipt.InstanceID || ref.IncarnationID == "" ||
		ref.ExecutionEpoch != receipt.ExpectedExecutionEpoch+1 || point.Stage != "released" || point.ActivationDecision != "commit" ||
		!statePointDigestValid(point.ManifestDigest) || point.RepresentationID == "" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "restore lacks confirmed epoch and identity activation"})
	}
	current, err := control.DiscoverExecution(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if current != *ref {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "the live replacement differs from the committed Worker execution"})
	}
	inventory, err := control.NativeInventory(ctx, current)
	if err != nil {
		return err
	}
	vm := inventory.Execution
	if vm.ExecutionRef != current || vm.BundleDigest != j.Binding.RuntimeBundleID || vm.Phase != "registered" ||
		vm.PID < 1 || vm.PIDStartTime < 1 || vm.HostBootID == "" || inventory.Inventory.SourceDisposition != "RUNNING" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "restore target has no verified running inventory"})
	}
	profile := ""
	if inventory.ManagedStandby() {
		profile = j.Binding.RuntimeBundleID + "/" + inventory.Inventory.Protocol
	}
	observation := nativeObservation(j.Binding, current, registry.PowerRunning, profile)
	observation.Native.VerifiedRestore = true
	if storage := inventory.ForkStorage(); storage != nil {
		observation.Native.ForkCapabilities = observedForkCapabilities(inventory)
		observation.Native.ForkStorage = storage
	}
	observation.RuntimeIdentity = JSON(rt.Identity{BootID: JSON(vm.HostBootID), PID: JSON(vm.PID), StartTime: JSON(vm.PIDStartTime)})
	result := map[string]any{"machine_id": j.Op.MachineID, "execution_epoch": current.ExecutionEpoch,
		"statepoint": point, "volume_map": point.VolumeMap}
	transition := registry.TransitionRequest{Observation: &observation, Effect: registry.EffectApplied, DesiredPower: "running",
		PromoteBinding: j.Op.Kind == rt.VMRestore}
	products, err := j.observeRestoredVolumes(ctx, inventory, point.VolumeMap, point.ProductAttachments)
	if err != nil {
		return err
	}
	if rescue != nil {
		transition.SourceDisposition = registry.SourceExited
		result["rescue_capture_id"] = rescue.RescueCaptureID
	}
	configuration := j.Intent.CaptureConfig
	if configuration != nil && configuration.ManifestDigest != point.ManifestDigest {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "restore target manifest differs from the captured configuration"})
	}
	if j.Op.Kind == rt.VMRestore || j.Op.Kind == rt.VMRollback {
		if configuration == nil {
			result["capture_configuration_status"] = "unknown"
		} else {
			result["capture_configuration_status"] = configuration.Status
		}
	}
	if rescue != nil || len(products) != 0 {
		transition.Apply = func(ctx context.Context, tx *sql.Tx) error {
			for _, product := range products {
				if err := volumes.Apply(ctx, tx, product.VolumeRoute, product.Name, j.Op.MachineID, j.Binding.ID,
					point.ProductAttachments[product.ID], "attach", product.VolumeObservation); err != nil {
					return err
				}
			}
			if rescue != nil {
				// Preserve identity, boot selector, and current authorization. A
				// historical rollback has no proof of its mutable VM settings.
				spec := json.RawMessage(`{}`)
				if configuration != nil {
					spec = configuration.RestoreMachineSpec()
				}
				if _, err := tx.ExecContext(ctx, `UPDATE machines SET spec_json=json_patch(
					json_remove(spec_json,'$.live_compute_growth','$.network','$.resources'), ?) WHERE id=?`,
					string(spec), j.Op.MachineID); err != nil {
					return err
				}
				return j.recordStatePoint(ctx, tx, *rescue, rescue.RescueCaptureID, "rollback_rescue")
			}
			return nil
		}
	}
	transition.Result = JSON(result)
	return j.Done(ctx, transition)
}
