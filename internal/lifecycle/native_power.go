package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (e *Engine) NativePowerCapability(binding registry.Binding, kind string) rt.Capability {
	capability := rt.FullStateCapabilities(false)[rt.NativePowerCapability(kind)]
	if binding.ExecutionEpoch == 0 || binding.RuntimeInstanceObjectID == "" || binding.IncarnationID == "" ||
		binding.CapabilityProfileID == "" || binding.ObservedAt == nil ||
		time.Since(*binding.ObservedAt) > time.Duration(e.Config.Lifecycle.StatusStale)*time.Second ||
		e.AdmitRuntime(bindingRuntime(binding).BundleRoute, kind) != nil {
		return capability
	}
	if kind == "stop" {
		var evidence struct {
			StopBarrier bool `json:"native_stop_barrier"`
		}
		if json.Unmarshal(binding.Health, &evidence) != nil || !evidence.StopBarrier {
			return capability
		}
	}
	return rt.Capability{Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: binding.CapabilityProfileID}
}

func (e *Engine) AdmitNativePower(binding registry.Binding, kind string) error {
	if kind == "stop" && binding.PowerState != registry.PowerRunning {
		return &Failure{"STATE_CONFLICT", "cold stop requires a running System execution"}
	}
	capability := e.NativePowerCapability(binding, kind)
	if !capability.Supported {
		return &rt.CapabilityError{Capability: rt.NativePowerCapability(kind), Status: capability}
	}
	if (kind == rt.Standby && binding.PowerState != registry.PowerRunning) ||
		(kind == rt.Wake && binding.PowerState != registry.PowerStandby) {
		return &Failure{"STATE_CONFLICT", "native operation requires its confirmed source power state"}
	}
	return nil
}

func nativeObservation(binding registry.Binding, ref rt.ExecutionRef, power registry.PowerState, profile string) registry.Observation {
	state := string(power)
	if power == registry.PowerStandby {
		state = "paused"
	}
	observation := registry.Observation{
		ObservedState: state, NativeState: string(power), Availability: "blocked",
		RuntimeIdentity: binding.RuntimeIdentity, ExpectedExecutionEpoch: &binding.ExecutionEpoch,
		GuestManagementStatus: "unknown", StateReason: json.RawMessage(`""`),
		Native: &registry.NativeObservation{Execution: ref, PowerState: power, VerifiedStandbyProfile: profile},
	}
	if binding.ExecutionRef() == ref {
		observation.GuestManagementEvidence = binding.GuestManagementEvidence
	}
	return observation
}

func (e *Engine) observeNativePower(ctx context.Context, binding registry.Binding, adapter rt.Adapter) bool {
	control, ok := adapter.(rt.NativeControl)
	if !ok {
		return false
	}
	ref := binding.ExecutionRef()
	if ref.ExecutionEpoch == 0 {
		var err error
		ref, err = control.DiscoverExecution(ctx, binding.RuntimeName)
		if err != nil {
			return false
		}
	}
	inventory, err := control.NativeInventory(ctx, ref)
	if err != nil {
		return false
	}
	var power registry.PowerState
	switch inventory.Inventory.SourceDisposition {
	case "RUNNING":
		power = registry.PowerRunning
	case "STANDBY":
		power = registry.PowerStandby
	default:
		return false
	}
	profile := ""
	if inventory.ManagedStandby() {
		profile = binding.RuntimeBundleID + "/" + inventory.Inventory.Protocol
	}
	observation := nativeObservation(binding, ref, power, profile)
	observation.Native.StopBarrier = &inventory.Inventory.StopBarrier
	observation.Native.ForkCapabilities = observedForkCapabilities(inventory)
	observation.Native.ForkStorage = inventory.ForkStorage()
	digest := e.observeSystemGuest(ctx, binding, adapter, &observation)
	if err := e.DB.ObserveMachine(ctx, binding.ID, observation); err != nil {
		return false
	}
	if digest != "" {
		e.queueNativeSSH(ctx, binding, digest)
	}
	return true
}

func (j *Job) nativePower(ctx context.Context, control rt.NativeControl) error {
	ref := j.Binding.ExecutionRef()
	inventory, err := control.NativeInventory(ctx, ref)
	if err != nil {
		return err
	}
	if !inventory.ManagedStandby() {
		return rt.NativePowerUnavailable(j.Op.Kind)
	}
	parameters := rt.NativeParameters{}
	if j.Op.Kind == "stop" {
		if !inventory.Inventory.StopBarrier {
			return rt.NativePowerUnavailable(j.Op.Kind)
		}
		if inventory.Inventory.SourceDisposition != "RUNNING" {
			return &Failure{"STATE_CONFLICT", "cold stop requires a running System execution"}
		}
	} else if j.Op.Kind == rt.Standby {
		if inventory.Inventory.SourceDisposition != "RUNNING" {
			return &Failure{"STATE_CONFLICT", "native execution is not running"}
		}
		if err := j.checkNativeStandbyActivity(ctx); err != nil {
			return err
		}
		parameters.Mode = "managed"
	} else if inventory.Inventory.SourceDisposition != "STANDBY" {
		return &Failure{"STATE_CONFLICT", "native execution is not in standby"}
	}
	request := rt.NativeSubmission{OperationID: j.Op.ID, Expected: ref, Kind: j.Op.Kind, Parameters: parameters}
	request.RequestDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(request)))
	j.Intent.Native = &request
	if err := j.Stage(ctx, "worker_submission", ""); err != nil {
		return err
	}
	return j.submitNative(ctx, control)
}

func (j *Job) submitNative(ctx context.Context, control rt.NativeControl) error {
	request := *j.Intent.Native
	receipt, err := control.SubmitNative(ctx, request)
	if err != nil {
		if refusal := nativeRefusal(err); refusal != nil {
			if request.Kind != "stop" {
				return refusal
			}
			known, queryErr := control.QueryOperation(ctx, request.OperationID)
			if queryErr == nil {
				j.Written = true
				return j.recordNativeReceipt(ctx, known)
			}
			if launchReceiptMissing(queryErr) {
				j.Written = false
				return refusal
			}
		}
		// Observe the original operation after a lost response. The Worker owns
		// an accepted transition independently of the Submit connection.
		return j.transition(ctx, registry.TransitionRequest{
			State: registry.Observing, Stage: j.Intent.Stage, Effect: registry.EffectUnknown,
		})
	}
	j.Written = true
	return j.recordNativeReceipt(ctx, receipt)
}

func (j *Job) checkNativeStandbyActivity(ctx context.Context) error {
	var request struct {
		ActivityPolicy struct {
			AllowActiveTasks bool `json:"allow_active_tasks"`
		} `json:"activity_policy"`
	}
	if err := json.Unmarshal(j.Op.Request, &request); err != nil {
		return err
	}
	j.mu.Lock()
	active := j.holds[j.Op.MachineID] > 0
	j.mu.Unlock()
	if active {
		return &Failure{"ACTIVE_ACTIVITY", "machine has active connections"}
	}
	var held bool
	err := j.DB.ReadDB().QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM leases WHERE machine_id=? AND binding_generation=? AND state!='released') OR
		EXISTS(SELECT 1 FROM exec_sessions WHERE binding_id=? AND state NOT IN ('done','exited','lost','failed')) OR
		(?=0 AND EXISTS(SELECT 1 FROM tasks WHERE task_namespace=? AND state NOT IN ('succeeded','failed','cancelled','interrupted')))`,
		j.Op.MachineID, j.Binding.Generation, j.Binding.ID, request.ActivityPolicy.AllowActiveTasks, j.Binding.TaskNamespace).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return &Failure{"ACTIVE_ACTIVITY", "machine has active tasks, sessions or leases"}
	}
	return nil
}

func nativeRefusal(err error) error {
	var fault *rt.Error
	if !errors.As(err, &fault) {
		return nil
	}
	var code string
	switch fault.NativeCode {
	case "E_CAPABILITY_UNSUPPORTED", "E_NATIVE_UNBOUND":
		code = "CAPABILITY_UNSUPPORTED"
	case "E_STALE_EXECUTION":
		code = "EXECUTION_EPOCH_CHANGED"
	case "E_STATE_CONFLICT":
		code = "STATE_CONFLICT"
	case "E_VOLUME_CONFLICT":
		code = "VOLUME_CONFLICT"
	case "E_OPERATION_IN_PROGRESS":
		code = "OPERATION_IN_PROGRESS"
	case "E_RESOURCE_LIMIT":
		code = "RESOURCE_LIMIT"
	case "E_PERMISSION_DENIED":
		code = "PERMISSION_DENIED"
	case "E_INVALID_REQUEST":
		code = "INVALID_ARGUMENT"
	default:
		return nil
	}
	return &Failure{code, fault.Message}
}

func (j *Job) reconcileNative(ctx context.Context, control rt.NativeControl) error {
	j.Written = true
	receipt, err := control.QueryOperation(ctx, j.Op.ID)
	if j.Op.Kind == "stop" && launchReceiptMissing(err) {
		if j.Intent.Native == nil || j.Intent.Native.Expected != j.Binding.ExecutionRef() {
			return j.fail(ctx, registry.ErrStaleObservation)
		}
		err = j.submitNative(ctx, control)
		if err != nil && !errors.Is(err, registry.ErrOperationState) && !errors.Is(err, rt.ErrUnavailable) {
			return j.fail(ctx, err)
		}
		return err
	}
	if err != nil {
		if errors.Is(err, rt.ErrUnavailable) {
			return err
		}
		return j.fail(ctx, err)
	}
	err = j.recordNativeReceipt(ctx, receipt)
	if err != nil && !errors.Is(err, registry.ErrOperationState) && (j.Op.Kind != "stop" || !errors.Is(err, rt.ErrUnavailable)) {
		return j.fail(ctx, err)
	}
	return err
}

func (j *Job) recordNativeReceipt(ctx context.Context, receipt rt.OperationReceipt) error {
	request := j.Intent.Native
	if request == nil || request.Expected != j.Binding.ExecutionRef() || receipt.Native == nil ||
		receipt.OperationID != j.Op.ID || receipt.RouteID != "deep" || receipt.BundleDigest != j.Binding.RuntimeBundleID ||
		receipt.Kind != request.Kind || receipt.RequestDigest != request.RequestDigest ||
		receipt.InstanceID != request.Expected.InstanceID || receipt.ExpectedExecutionEpoch != request.Expected.ExecutionEpoch ||
		receipt.ExpectedIncarnationID == nil || *receipt.ExpectedIncarnationID != request.Expected.IncarnationID {
		return &Failure{"RECOVERY_REQUIRED", "Worker receipt is outside the accepted execution or intent"}
	}
	progress := receipt.Native
	disposition := registry.SourceDisposition(progress.SourceDisposition)
	result := registry.TransitionRequest{SourceDisposition: disposition}
	switch receipt.Phase {
	case "NEW":
		if j.Op.State == registry.Observing && j.Op.Stage == progress.Stage && j.Op.SourceDisposition == disposition {
			return nil
		}
		result.State, result.Stage = registry.Observing, progress.Stage
		return j.transition(ctx, result)
	case "COMPLETED":
		if request.Kind == "stop" {
			return j.confirmNativeStop(ctx, receipt)
		}
		want, power := registry.SourceStandby, registry.PowerStandby
		if request.Kind == rt.Wake {
			want, power = registry.SourceRunning, registry.PowerRunning
		}
		knownMode := progress.Mode == "managed" || (request.Kind == rt.Wake && progress.Mode == "raw")
		if disposition != want || progress.Stage != "complete" || receipt.Decision != "NONE" || !knownMode {
			return &Failure{"RECOVERY_REQUIRED", "Worker completion differs from the accepted power transition"}
		}
		observation := nativeObservation(j.Binding, request.Expected, power, j.Binding.CapabilityProfileID)
		result.Effect, result.Observation = registry.EffectApplied, &observation
		result.Result = JSON(map[string]any{
			"machine_id": j.Op.MachineID, "mode": progress.Mode, "execution_epoch": request.Expected.ExecutionEpoch,
		})
		return j.Done(ctx, result)
	case "ABORTED":
		want := registry.SourceRunning
		if request.Kind == rt.Wake {
			want = registry.SourceStandby
		}
		if progress.Stage != "aborted" || receipt.Decision != "ABORT" || disposition != want {
			return &Failure{"RECOVERY_REQUIRED", "Worker abort has no confirmed stable disposition"}
		}
		result.Effect = registry.EffectNone
		result.Error = JSON(map[string]string{"code": "NATIVE_OPERATION_ABORTED", "message": "Worker aborted the transition"})
		return j.finish(ctx, registry.Failed, result)
	default:
		return &Failure{"RECOVERY_REQUIRED", "Worker returned an unrecognized native operation phase"}
	}
}
