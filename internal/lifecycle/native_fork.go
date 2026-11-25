package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type nativeForkIntent struct {
	Submission        rt.ForkSubmission          `json:"submission"`
	SourceDisks       []rt.ForkDiskEvidence      `json:"source_disks"`
	SourceDisposition registry.SourceDisposition `json:"source_disposition"`
	Receipt           *rt.OperationReceipt       `json:"receipt,omitempty"`
	RetryRequested    bool                       `json:"retry_requested,omitempty"`
	RetryFrom         *rt.OperationReceipt       `json:"retry_from,omitempty"`
}

func observedForkCapabilities(inventory rt.NativeInventory) map[string]rt.Capability {
	result := map[string]rt.Capability{}
	for _, kind := range []string{rt.IndependentFork, rt.ForkBatch} {
		if inventory.ForkEligible(kind) {
			capability := rt.FullStateOperationCapability(kind)
			result[capability] = inventory.Capabilities[capability]
		}
	}
	return result
}

func (e *Engine) NativeForkCapability(b registry.Binding, kind string) rt.Capability {
	name := rt.FullStateOperationCapability(kind)
	unknown := rt.FullStateCapabilities(false)[name]
	if b.ExecutionEpoch == 0 || b.RuntimeInstanceObjectID == "" || b.IncarnationID == "" || b.ObservedAt == nil ||
		time.Since(*b.ObservedAt) > time.Duration(e.Config.Lifecycle.StatusStale)*time.Second ||
		e.AdmitRuntime(bindingRuntime(b).BundleRoute, kind) != nil {
		return unknown
	}
	var evidence struct {
		Capabilities map[string]rt.Capability `json:"native_fork_capabilities"`
	}
	if json.Unmarshal(b.Health, &evidence) == nil && rt.VerifiedForkProfile(evidence.Capabilities[name]) {
		return evidence.Capabilities[name]
	}
	return unknown
}

func (e *Engine) AdmitNativeFork(b registry.Binding, kind string) error {
	capability := e.NativeForkCapability(b, kind)
	if !capability.Supported {
		return &rt.CapabilityError{Capability: rt.FullStateOperationCapability(kind), Status: capability}
	}
	if b.PowerState != registry.PowerRunning && b.PowerState != registry.PowerStandby {
		return &Failure{"STATE_CONFLICT", "native fork requires a running or managed standby source"}
	}
	return nil
}

func (j *Job) driveNativeFork(ctx context.Context, routeError error) error {
	control, ok := j.Runtime.(rt.NativeForkControl)
	if routeError != nil || !ok || bindingRuntime(j.Binding).Legacy() {
		if j.Op.State == registry.Queued {
			if err := j.Stage(ctx, "preflight", ""); err != nil {
				return err
			}
		}
		j.Written = j.Intent.Fork != nil
		return j.fail(ctx, rt.FullStateUnavailable(j.Op.Kind, bindingRuntime(j.Binding).Legacy()))
	}
	op, err := j.DB.GetOperation(ctx, j.Op.ID)
	if err != nil {
		return err
	}
	if op.Version != j.Op.Version {
		return registry.ErrOperationState
	}
	j.Op = op
	if j.Intent.Fork != nil {
		return j.reconcileNativeFork(ctx, control)
	}
	if j.Op.State != registry.Queued && j.Intent.Stage != "preflight" {
		j.Written = true
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "dispatched fork has no durable Worker intent"})
	}
	if err := readyRuntime(ctx, j.Runtime); err != nil {
		return err
	}
	if err := j.prepareDispatch(ctx); err != nil || j.Op.Terminal() {
		return err
	}
	ref := j.Binding.ExecutionRef()
	inventory, err := control.NativeInventory(ctx, ref)
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	if inventory.Execution.ExecutionRef != ref || !inventory.ForkEligible(j.Op.Kind) {
		return j.fail(ctx, rt.FullStateUnavailable(j.Op.Kind, false))
	}
	request := rt.ForkSubmission{OperationID: j.Op.ID, Expected: ref, Kind: j.Op.Kind}
	if j.Op.Kind == rt.ForkBatch && len(j.Op.Request) != 0 {
		var input struct {
			Admission string `json:"admission"`
		}
		if err := json.Unmarshal(j.Op.Request, &input); err != nil {
			return j.fail(ctx, registry.ErrInvalid)
		}
		request.Parameters.Admission = input.Admission
	}
	for _, child := range j.Op.Children {
		binding, err := j.DB.GetBinding(ctx, child.BindingID)
		if err != nil {
			return j.fail(ctx, err)
		}
		request.Parameters.Children = append(request.Parameters.Children, binding.RuntimeName)
	}
	request.RequestDigest = nativeForkDigest(request)
	j.Intent.Fork = &nativeForkIntent{
		Submission: request, SourceDisks: inventory.ForkDisks(), SourceDisposition: registry.SourceDisposition(inventory.Inventory.SourceDisposition),
	}
	if err := j.Stage(ctx, "worker_fork", ""); err != nil {
		return err
	}
	return j.submitNativeFork(ctx, control)
}

func nativeForkDigest(request rt.ForkSubmission) string {
	request.RequestDigest = ""
	return fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(request)))
}

func (j *Job) submitNativeFork(ctx context.Context, control rt.NativeForkControl) error {
	prior, err := j.checkGuestSubmission(ctx, control.QueryOperation)
	if err != nil || j.Op.Terminal() {
		return err
	}
	if prior != nil {
		return j.confirmNativeFork(ctx, control, *prior)
	}
	receipt, err := control.SubmitFork(ctx, j.Intent.Fork.Submission)
	if err == nil {
		return j.confirmNativeFork(ctx, control, receipt)
	}
	refusal := nativeRefusal(err)
	var capability *rt.CapabilityError
	if refusal != nil || errors.As(err, &capability) {
		observed, queryErr := control.QueryOperation(ctx, j.Op.ID)
		if queryErr == nil {
			return j.confirmNativeFork(ctx, control, observed)
		}
		if launchReceiptMissing(queryErr) {
			if refusal == nil {
				refusal = capability
			}
			j.Written = false
			return j.fail(ctx, refusal)
		}
	}
	return j.recordForkProgress(ctx, registry.TransitionRequest{State: registry.Observing, Stage: "worker_fork", Effect: registry.EffectUnknown})
}

func (j *Job) reconcileNativeFork(ctx context.Context, control rt.NativeForkControl) error {
	j.Written = true
	intent := j.Intent.Fork
	request := intent.Submission
	if request.OperationID != j.Op.ID || request.Kind != j.Op.Kind || request.RequestDigest != nativeForkDigest(request) ||
		len(request.Parameters.Children) != len(j.Op.Children) || (!j.Op.SourceLockReleased && request.Expected != j.Binding.ExecutionRef()) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "native fork intent differs from the durable source or child set"})
	}
	receipt, err := control.QueryOperation(ctx, j.Op.ID)
	if launchReceiptMissing(err) && !j.Op.SourceLockReleased {
		return j.submitNativeFork(ctx, control)
	}
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	if intent.RetryRequested {
		return j.reconcileForkRetry(ctx, control, receipt)
	}
	return j.confirmNativeFork(ctx, control, receipt)
}

func (j *Job) checkForkReceipt(receipt rt.OperationReceipt) error {
	intent := j.Intent.Fork
	request := intent.Submission
	progress := receipt.Fork
	if progress == nil || receipt.OperationID != j.Op.ID || receipt.Kind != request.Kind || receipt.RequestDigest != request.RequestDigest ||
		receipt.RouteID != "deep" || receipt.BundleDigest != j.Binding.RuntimeBundleID || receipt.InstanceID != request.Expected.InstanceID ||
		receipt.ExpectedExecutionEpoch != request.Expected.ExecutionEpoch || receipt.ExpectedIncarnationID == nil ||
		*receipt.ExpectedIncarnationID != request.Expected.IncarnationID || progress.CaptureID == "" || progress.Stage == "" ||
		len(progress.Children) != len(j.Op.Children) || rt.ForkAdmission(progress.Admission) != rt.ForkAdmission(request.Parameters.Admission) ||
		progress.OriginalSourceDisposition != string(intent.SourceDisposition) ||
		(j.Op.SourceLockReleased && !progress.SourceReleased) {
		return &Failure{"RECOVERY_REQUIRED", "Worker fork receipt is outside the accepted source or cut"}
	}
	var old struct {
		CaptureID string `json:"capture_id"`
	}
	if len(j.Op.Result) != 0 && (json.Unmarshal(j.Op.Result, &old) != nil || old.CaptureID != progress.CaptureID) {
		return &Failure{"RECOVERY_REQUIRED", "Worker fork capture changed after admission"}
	}
	return verifyForkReceiptChildren(request, progress.Children)
}

func (j *Job) confirmNativeFork(ctx context.Context, control rt.NativeForkControl, receipt rt.OperationReceipt) error {
	j.Written = true
	if err := j.checkForkReceipt(receipt); err != nil {
		return j.fail(ctx, err)
	}
	intent, progress := j.Intent.Fork, receipt.Fork
	if previous := intent.Receipt; previous != nil && (previous.Fork == nil || len(previous.Fork.Children) != len(progress.Children) || receipt.Revision < previous.Revision) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork receipt revision regressed"})
	}
	for ordinal, child := range progress.Children {
		if child.Attempt != j.Op.Children[ordinal].Attempt || (intent.Receipt == nil && child.ExecutionEpoch != 1) ||
			(intent.Receipt != nil && !sameForkAttempt(child, intent.Receipt.Fork.Children[ordinal])) {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork changed a child outside a recorded retry"})
		}
	}
	admitted := make([]string, 0, len(progress.Children))
	for ordinal, child := range progress.Children {
		if child.AdmissionError == "" {
			admitted = append(admitted, j.Op.Children[ordinal].MachineID)
		}
	}
	result := map[string]any{"machine_id": j.Op.MachineID, "capture_id": progress.CaptureID,
		"admission": rt.ForkAdmission(intent.Submission.Parameters.Admission), "admitted_child_ids": admitted}
	intent.Receipt = &receipt
	step := registry.TransitionRequest{State: registry.Observing, Stage: progress.Stage, Effect: registry.EffectUnknown, Result: JSON(result), Dispatch: JSON(j.Intent)}
	if receipt.Phase == "ABORTED" && receipt.Decision == "ABORT" && !j.Op.SourceLockReleased {
		return j.confirmForkAbort(ctx, control, receipt, step)
	}
	if (receipt.Phase != "NEW" && receipt.Phase != "COMPLETED") || receipt.Decision != "NONE" ||
		(receipt.Phase == "COMPLETED" && (!progress.SourceReleased || progress.Stage != "complete")) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork phase has an unrecognized disposition"})
	}
	if !progress.SourceReleased {
		return j.recordForkProgress(ctx, step)
	}
	step.SourceDisposition = intent.SourceDisposition
	if !j.Op.SourceLockReleased {
		step.ReleaseSourceLock = true
		return j.recordForkProgress(ctx, step)
	}
	return j.confirmForkChildren(ctx, control, receipt, step, result)
}

func (j *Job) confirmForkAbort(ctx context.Context, control rt.NativeForkControl, receipt rt.OperationReceipt, step registry.TransitionRequest) error {
	progress := receipt.Fork
	step.Effect = registry.EffectNone
	state := registry.Failed
	code := "NATIVE_FORK_ABORTED"
	if progress.SourceReleased && progress.Stage == "aborted" {
		cancelled := progress.Outcome == "cancelled" && progress.Error == nil
		for ordinal, child := range progress.Children {
			if child.Phase != "ABORTED" && child.Phase != "RESERVED" {
				return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "aborted fork still has a dispatched child"})
			}
			cancelled = cancelled && (child.Cancelled || child.AdmissionError != "")
			childCode := code
			if child.AdmissionError != "" {
				childCode = "RESOURCE_LIMIT"
			}
			step.ForkChildren = append(step.ForkChildren, registry.ForkChildTransition{Ordinal: ordinal, State: "quarantined", ErrorCode: childCode})
		}
		inventory, err := control.NativeInventory(ctx, j.Intent.Fork.Submission.Expected)
		if errors.Is(err, rt.ErrUnavailable) {
			return err
		}
		if err == nil && inventory.Execution.ExecutionRef == j.Intent.Fork.Submission.Expected &&
			inventory.Inventory.SourceDisposition == string(j.Intent.Fork.SourceDisposition) {
			step.SourceDisposition = j.Intent.Fork.SourceDisposition
			step.ReleaseSourceLock = true
		} else {
			step.SourceDisposition = registry.SourceUnknown
		}
		if cancelled {
			state, code = registry.Cancelled, "FORK_CANCELLED"
			for i := range step.ForkChildren {
				if step.ForkChildren[i].ErrorCode != "RESOURCE_LIMIT" {
					step.ForkChildren[i].ErrorCode = code
				}
			}
		}
	}
	step.Error = JSON(map[string]string{"code": code, "message": "Worker ended the fork before a child was published"})
	return j.finish(ctx, state, step)
}

func verifyForkReceiptChildren(request rt.ForkSubmission, children []rt.ForkChildReceipt) error {
	instances, operations := map[string]bool{}, map[string]bool{}
	for ordinal, child := range children {
		if child.Name != request.Parameters.Children[ordinal] || child.OperationID == "" || child.InstanceID == "" ||
			child.OperationID == request.OperationID || child.InstanceID == request.Expected.InstanceID ||
			instances[child.InstanceID] || operations[child.OperationID] || child.ExecutionEpoch < 1 || child.Attempt < 0 {
			return &Failure{"RECOVERY_REQUIRED", "Worker fork child differs from the accepted ordered child set"}
		}
		if child.AdmissionError != "" && (request.Parameters.Admission != rt.ForkBestEffort || child.AdmissionError != "E_RESOURCE_LIMIT" ||
			child.Phase != "ABORTED" || child.IncarnationID != nil || child.ExecutionEpoch != 1 || child.Attempt != 0 || child.Cancelled) {
			return &Failure{"RECOVERY_REQUIRED", "Worker fork child reports an inconsistent budget refusal"}
		}
		instances[child.InstanceID], operations[child.OperationID] = true, true
	}
	return nil
}

func sameForkAttempt(a, b rt.ForkChildReceipt) bool {
	return a.Name == b.Name && a.InstanceID == b.InstanceID && a.OperationID == b.OperationID &&
		a.ExecutionEpoch == b.ExecutionEpoch && a.Attempt == b.Attempt && a.AdmissionError == b.AdmissionError &&
		(b.IncarnationID == nil || (a.IncarnationID != nil && *a.IncarnationID == *b.IncarnationID)) && (!b.Cancelled || a.Cancelled)
}

func (j *Job) recordForkProgress(ctx context.Context, step registry.TransitionRequest) error {
	if !step.ReleaseSourceLock && len(step.ForkChildren) == 0 && len(step.ForkRetryOrdinals) == 0 &&
		j.Op.State == step.State && j.Op.Stage == step.Stage && j.Op.Effect == step.Effect && j.Op.SourceDisposition == step.SourceDisposition &&
		bytes.Equal(j.Op.Result, step.Result) && (len(step.Dispatch) == 0 || bytes.Equal(j.Op.Dispatch, step.Dispatch)) {
		return nil
	}
	return j.transition(ctx, step)
}

func (j *Job) confirmForkChildren(ctx context.Context, control rt.NativeForkControl, receipt rt.OperationReceipt, step registry.TransitionRequest, result map[string]any) error {
	proof := append([]rt.ForkDiskEvidence(nil), j.Intent.Fork.SourceDisks...)
	ready, failed := 0, 0
	for _, child := range j.Op.Children {
		if child.State == "ready" {
			proof = append(proof, child.ResourceProof...)
			ready++
		} else if child.State == "quarantined" {
			failed++
		}
	}
	var conflict error
	for ordinal, child := range j.Op.Children {
		if child.State != "pending" {
			continue
		}
		workerChild := receipt.Fork.Children[ordinal]
		switch workerChild.Phase {
		case "RESERVED", "NEW":
			if receipt.Phase == "COMPLETED" {
				conflict = &Failure{"RECOVERY_REQUIRED", "Worker completed the fork with a pending child"}
			}
		case "ABORTED":
			code := "NATIVE_CHILD_ABORTED"
			if workerChild.AdmissionError != "" {
				code = "RESOURCE_LIMIT"
			} else if workerChild.Cancelled {
				code = "FORK_CHILD_CANCELLED"
			}
			step.ForkChildren = append(step.ForkChildren, registry.ForkChildTransition{Ordinal: ordinal, State: "quarantined", ErrorCode: code})
			failed++
		case "COMPLETED":
			change, err := j.confirmForkChild(ctx, control, child, workerChild, proof)
			if errors.Is(err, rt.ErrUnavailable) {
				continue
			}
			if err != nil {
				conflict = err
				continue
			}
			step.ForkChildren = append(step.ForkChildren, change)
			proof = append(proof, change.ResourceProof...)
			ready++
		default:
			conflict = &Failure{"RECOVERY_REQUIRED", "Worker returned an unrecognized child phase"}
		}
	}
	if ready > 0 {
		step.Effect = registry.EffectPartial
		if j.Op.Kind == rt.IndependentFork {
			result["child_machine_id"] = j.Op.Children[0].MachineID
			step.Result = JSON(result)
		}
	}
	if conflict != nil {
		if err := j.recordForkProgress(ctx, step); err != nil {
			return err
		}
		return j.fail(ctx, conflict)
	}
	if receipt.Phase != "COMPLETED" || ready+failed != len(j.Op.Children) {
		return j.recordForkProgress(ctx, step)
	}
	if ready == 0 {
		step.Effect = registry.EffectNone
		step.Error = JSON(map[string]string{"code": "NATIVE_FORK_NO_CHILDREN", "message": "Worker completed the fork without a ready child"})
		return j.finish(ctx, registry.Failed, step)
	}
	if failed == 0 {
		step.Effect = registry.EffectApplied
	}
	return j.Done(ctx, step)
}

func (j *Job) confirmForkChild(ctx context.Context, control rt.NativeForkControl, child registry.OperationChild, receipt rt.ForkChildReceipt, priorDisks []rt.ForkDiskEvidence) (registry.ForkChildTransition, error) {
	var result registry.ForkChildTransition
	if receipt.IncarnationID == nil || *receipt.IncarnationID == "" || *receipt.IncarnationID == j.Intent.Fork.Submission.Expected.IncarnationID {
		return result, &Failure{"RECOVERY_REQUIRED", "Worker child has no independent incarnation"}
	}
	binding, err := j.DB.GetBinding(ctx, child.BindingID)
	if err != nil {
		return result, err
	}
	ref, err := control.DiscoverExecution(ctx, binding.RuntimeName)
	if err != nil {
		return result, err
	}
	want := rt.ExecutionRef{InstanceID: receipt.InstanceID, IncarnationID: *receipt.IncarnationID, ExecutionEpoch: receipt.ExecutionEpoch}
	if ref != want || binding.RuntimeName != receipt.Name {
		return result, &Failure{"RECOVERY_REQUIRED", "registered child differs from its Worker receipt"}
	}
	inventory, err := control.NativeInventory(ctx, ref)
	if err != nil {
		return result, err
	}
	vm := inventory.Execution
	disks := inventory.ForkDisks()
	if vm.ExecutionRef != ref || vm.BundleDigest != binding.RuntimeBundleID || vm.Phase != "registered" ||
		vm.PID < 1 || vm.PIDStartTime < 1 || vm.HostBootID == "" || inventory.Inventory.SourceDisposition != "RUNNING" ||
		!independentForkDisks(j.Intent.Fork.SourceDisks, priorDisks, disks) {
		return result, &Failure{"RECOVERY_REQUIRED", "child has no verified live inventory with independent block writers"}
	}
	profile := ""
	if inventory.ManagedStandby() {
		profile = binding.RuntimeBundleID + "/" + inventory.Inventory.Protocol
	}
	x := nativeObservation(binding, ref, registry.PowerRunning, profile)
	x.RuntimeIdentity = JSON(rt.Identity{BootID: JSON(vm.HostBootID), PID: JSON(vm.PID), StartTime: JSON(vm.PIDStartTime)})
	x.Native.ForkCapabilities = observedForkCapabilities(inventory)
	return registry.ForkChildTransition{Ordinal: child.Ordinal, State: "ready", Observation: &x, ResourceProof: disks}, nil
}

func independentForkDisks(source, previous, child []rt.ForkDiskEvidence) bool {
	if len(source) == 0 || len(source) != len(child) {
		return false
	}
	devices := make(map[string]rt.ForkDiskEvidence, len(source))
	for _, disk := range source {
		devices[disk.DeviceID] = disk
	}
	volumes, heads := map[string]bool{}, map[string]bool{}
	for _, disk := range previous {
		if !disk.ReadOnly {
			volumes[disk.VolumeID], heads[disk.WritableHeadID] = true, true
		}
	}
	for _, disk := range child {
		original, ok := devices[disk.DeviceID]
		if !ok || original.ReadOnly != disk.ReadOnly || original.Role != disk.Role {
			return false
		}
		if !disk.ReadOnly {
			if volumes[disk.VolumeID] || heads[disk.WritableHeadID] {
				return false
			}
			volumes[disk.VolumeID], heads[disk.WritableHeadID] = true, true
		}
	}
	return true
}
