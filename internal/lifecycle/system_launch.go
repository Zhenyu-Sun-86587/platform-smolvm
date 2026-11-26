package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
	"sandbox-platform/internal/volumes"
)

func (e *Engine) SystemBootCapability(ctx context.Context, route rt.BundleRoute) (rt.Capability, error) {
	boot, _, err := e.SystemLaunchCapabilities(ctx, route)
	return boot, err
}

func (e *Engine) SystemLaunchCapabilities(ctx context.Context, route rt.BundleRoute) (rt.Capability, rt.Capability, error) {
	unknown := rt.FullStateCapabilities(false)[rt.SystemBootCapability]
	adapter, err := e.runtimes.ForBinding(rt.BindingRuntime{BundleRoute: route})
	launcher, ok := adapter.(rt.NativeLauncher)
	if err != nil || !ok {
		return unknown, unknown, nil
	}
	if profiles, ok := adapter.(interface {
		SystemLaunchCapabilities(context.Context) (rt.Capability, rt.Capability, error)
	}); ok {
		return profiles.SystemLaunchCapabilities(ctx)
	}
	capability, err := launcher.SystemBoot(ctx)
	if err != nil {
		return unknown, unknown, err
	}
	return capability, unknown, nil
}

func (request Create) ValidateSystemCreate() error {
	if request.LiveComputeGrowth && request.Resources.CPUs > 16 {
		return fmt.Errorf("%w: live compute growth supports up to 16 vCPU slots", registry.ErrInvalid)
	}
	if request.Resources.Storage != 0 || request.Resources.Overlay != 0 {
		return fmt.Errorf("%w: the System root disk capacity is determined by its template", registry.ErrInvalid)
	}
	if request.IdlePolicy != nil && request.IdlePolicy.Enabled {
		return fmt.Errorf("%w: System creation requires automatic idle to be disabled", registry.ErrInvalid)
	}
	return nil
}

func (e *Engine) systemLaunchSpec(ctx context.Context, request Create) (templates.Spec, error) {
	template, err := e.DB.GetTemplate(ctx, request.TemplateID)
	if err != nil {
		return templates.Spec{}, err
	}
	spec, err := templates.ReadSpec(template)
	if err != nil {
		return spec, err
	}
	if spec.BootMode != rt.BootSystem {
		return spec, registry.ErrInvalid
	}
	return spec, request.ValidateSystemCreate()
}

// FirstSystemLaunch derives the first launch from the accepted machine and its
// authoritative offline attachments. The Worker still authorizes the actual slot.
func (e *Engine) FirstSystemLaunch(ctx context.Context, m registry.Machine, b registry.Binding) (rt.LaunchParameters, error) {
	if m.CurrentBinding == nil || m.CurrentBinding.ID != b.ID || m.DeletedAt != nil ||
		b.PowerState != registry.PowerStopped || b.ExecutionEpoch != 0 || b.IncarnationID != "" || b.RuntimeInstanceObjectID == "" {
		return rt.LaunchParameters{}, registry.ErrStaleObservation
	}
	var request Create
	if err := json.Unmarshal(m.Spec, &request); err != nil {
		return rt.LaunchParameters{}, err
	}
	request.TemplateID = m.TemplateID
	spec, err := e.systemLaunchSpec(ctx, request)
	if err != nil {
		return rt.LaunchParameters{}, err
	}
	return e.systemLaunchParameters(ctx, b, b.RuntimeInstanceObjectID, request, spec.RootGenerationID)
}

func (e *Engine) systemLaunchParameters(ctx context.Context, b registry.Binding, instance string, request Create, root string) (rt.LaunchParameters, error) {
	if b.WorkerID != "" && b.WorkerID != registry.LocalWorkerID {
		template, err := e.DB.GetTemplate(ctx, request.TemplateID)
		if err != nil {
			return rt.LaunchParameters{}, err
		}
		root, err = e.DB.TemplateGeneration(ctx, b, template.ImageDigest)
		if err != nil {
			return rt.LaunchParameters{}, err
		}
		adapter, err := e.ForBinding(b)
		if err != nil {
			return rt.LaunchParameters{}, err
		}
		networks, ok := adapter.(interface {
			ResolveLaunchNetwork(context.Context, *rt.LaunchNetwork) (*rt.LaunchNetwork, error)
		})
		if !ok {
			return rt.LaunchParameters{}, rt.ErrUnsupported
		}
		request.Network, err = networks.ResolveLaunchNetwork(ctx, request.Network)
		if err != nil {
			return rt.LaunchParameters{}, err
		}
	}
	route := rt.VolumeRoute{RuntimeInstanceID: b.RuntimeInstanceID, RuntimeBundleID: b.RuntimeBundleID}
	attachments, err := volumes.LaunchDisks(ctx, e.DB.ReadDB(), b.MachineID, instance, route)
	if err != nil {
		return rt.LaunchParameters{}, err
	}
	disks := []rt.LaunchDisk{{AttachmentID: b.ID + "-root", DeviceID: "root", Role: "root", GenerationID: root}}
	return rt.LaunchParameters{
		Name: b.RuntimeName, BootProfile: rt.SystemBootProfile, CPUs: request.Resources.CPUs,
		MemoryMiB: request.Resources.Memory, Init: "/sbin/init", Args: []string{},
		Network: request.Network, LiveComputeGrowth: request.LiveComputeGrowth, Disks: append(disks, attachments...),
	}, nil
}

func (j *Job) driveSystemLaunch(ctx context.Context, request Create, routeError error) error {
	launcher, ok := j.Runtime.(rt.NativeLauncher)
	if routeError != nil || !ok || bindingRuntime(j.Binding).Legacy() {
		if j.Op.State == registry.Queued {
			if err := j.Stage(ctx, "preflight", ""); err != nil {
				return err
			}
		}
		j.Written = j.Intent.Launch != nil
		return j.fail(ctx, rt.SystemBootUnavailable(bindingRuntime(j.Binding).Legacy()))
	}
	if j.Intent.Launch != nil {
		return j.reconcileSystemLaunch(ctx, launcher)
	}
	if err := readyRuntime(ctx, j.Runtime); err != nil {
		return err
	}
	if err := j.prepareDispatch(ctx); err != nil || j.Op.Terminal() {
		return err
	}
	spec, err := j.systemLaunchSpec(ctx, request)
	if err != nil {
		return j.fail(ctx, err)
	}
	binding, err := launcher.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	if binding.IncarnationID != nil || binding.ExecutionEpoch != j.Binding.ExecutionEpoch ||
		(j.Binding.RuntimeInstanceObjectID != "" && j.Binding.RuntimeInstanceObjectID != binding.InstanceID) ||
		j.Binding.PowerState == registry.PowerHibernated {
		return j.fail(ctx, &Failure{"STATE_CONFLICT", "System cold launch requires the exact stopped Worker binding"})
	}
	if j.Op.Kind == "create" && !request.Start {
		ref := rt.ExecutionRef{InstanceID: binding.InstanceID}
		observation := nativeObservation(j.Binding, ref, registry.PowerStopped, "")
		observation.Native.VerifiedLaunch = true
		return j.Done(ctx, registry.TransitionRequest{Observation: &observation, Effect: registry.EffectApplied,
			PromoteBinding: true, DesiredPower: "stopped", Result: JSON(map[string]any{"machine_id": j.Op.MachineID, "boot_mode": rt.BootSystem})})
	}
	parameters, err := j.systemLaunchParameters(ctx, j.Binding, binding.InstanceID, request, spec.RootGenerationID)
	if err != nil {
		return j.fail(ctx, err)
	}
	intent := rt.LaunchSubmission{OperationID: j.Op.ID, Expected: binding, Kind: rt.Launch, Parameters: parameters}
	intent.RequestDigest = launchDigest(intent)
	j.Intent.Launch = &intent
	if err := j.Stage(ctx, "worker_launch", "running"); err != nil {
		return err
	}
	return j.submitSystemLaunch(ctx, launcher)
}

func launchDigest(request rt.LaunchSubmission) string {
	request.RequestDigest = ""
	return fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(request)))
}

func launchReceiptMissing(err error) bool {
	var fault *rt.Error
	return errors.As(err, &fault) && errors.Is(fault, rt.ErrNotFound) && fault.NativeCode == "E_NOT_FOUND"
}

func (j *Job) submitSystemLaunch(ctx context.Context, launcher rt.NativeLauncher) error {
	prior, err := j.checkGuestSubmission(ctx, launcher.QueryOperation)
	if err != nil || j.Op.Terminal() {
		return err
	}
	if prior != nil {
		return j.confirmSystemLaunch(ctx, launcher, *prior)
	}
	receipt, err := launcher.SubmitLaunch(ctx, *j.Intent.Launch)
	if err == nil {
		return j.confirmSystemLaunch(ctx, launcher, receipt)
	}
	// A local capability check never sends the mutation. A Worker refusal
	// can be terminal only when the durable operation is absent as well.
	refusal := nativeRefusal(err)
	var capability *rt.CapabilityError
	if refusal != nil || errors.As(err, &capability) {
		known, queryErr := launcher.QueryOperation(ctx, j.Op.ID)
		if queryErr == nil {
			return j.confirmSystemLaunch(ctx, launcher, known)
		}
		if launchReceiptMissing(queryErr) {
			if refusal == nil {
				refusal = capability
			}
			j.Written = false
			return j.fail(ctx, refusal)
		}
	}
	return j.awaitSystemLaunch(ctx)
}

func (j *Job) reconcileSystemLaunch(ctx context.Context, launcher rt.NativeLauncher) error {
	j.Written = true
	request := j.Intent.Launch
	if request.OperationID != j.Op.ID || request.Kind != rt.Launch || request.Parameters.Name != j.Binding.RuntimeName ||
		request.Expected.ExecutionEpoch != j.Binding.ExecutionEpoch || request.Expected.IncarnationID != nil || request.RequestDigest != launchDigest(*request) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "System launch intent does not match the accepted operation"})
	}
	receipt, err := launcher.QueryOperation(ctx, j.Op.ID)
	if launchReceiptMissing(err) {
		// The same ID and digest are safe even if the original submit happens
		// to finish while this retry reaches the Worker.
		return j.submitSystemLaunch(ctx, launcher)
	}
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	return j.confirmSystemLaunch(ctx, launcher, receipt)
}

func (j *Job) awaitSystemLaunch(ctx context.Context) error {
	if j.Op.State == registry.Observing && j.Op.Stage == "worker_launch" && j.Op.Effect == registry.EffectUnknown {
		return nil
	}
	return j.transition(ctx, registry.TransitionRequest{State: registry.Observing, Stage: "worker_launch", Effect: registry.EffectUnknown})
}

func (j *Job) confirmSystemLaunch(ctx context.Context, launcher rt.NativeLauncher, receipt rt.OperationReceipt) error {
	j.Written = true
	request := j.Intent.Launch
	if receipt.OperationID != request.OperationID || receipt.Kind != rt.Launch || receipt.RequestDigest != request.RequestDigest ||
		receipt.RouteID != "deep" || receipt.BundleDigest != j.Binding.RuntimeBundleID || receipt.InstanceID != request.Expected.InstanceID ||
		receipt.ExpectedExecutionEpoch != request.Expected.ExecutionEpoch || receipt.ExpectedIncarnationID != nil {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker launch receipt does not match the accepted binding or intent"})
	}
	switch receipt.Phase {
	case "NEW":
		return j.awaitSystemLaunch(ctx)
	case "ABORTED":
		if receipt.Decision != "ABORT" || receipt.Execution != nil {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker launch abort has no confirmed final disposition"})
		}
		return j.finish(ctx, registry.Failed, registry.TransitionRequest{
			Effect: registry.EffectNone, Error: JSON(map[string]string{"code": "NATIVE_OPERATION_ABORTED", "message": "Worker aborted the System launch"}),
		})
	case "COMPLETED":
		return j.promoteSystemExecution(ctx, launcher, receipt)
	default:
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker returned an unrecognized System launch phase"})
	}
}

func (j *Job) promoteSystemExecution(ctx context.Context, launcher rt.NativeLauncher, receipt rt.OperationReceipt) error {
	expected := j.Intent.Launch.Expected
	if receipt.Decision != "NONE" || receipt.Execution == nil || receipt.Execution.InstanceID != expected.InstanceID ||
		receipt.Execution.ExecutionEpoch != expected.ExecutionEpoch+1 || receipt.Execution.IncarnationID == "" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker launch completion has no confirmed next execution"})
	}
	ref, err := launcher.DiscoverExecution(ctx, j.Binding.RuntimeName)
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	if ref != *receipt.Execution {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "registered execution differs from the Worker launch completion"})
	}
	inventory, err := launcher.NativeInventory(ctx, ref)
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil {
		return j.fail(ctx, err)
	}
	vm := inventory.Execution
	if vm.ExecutionRef != ref || vm.BundleDigest != j.Binding.RuntimeBundleID || vm.Phase != "registered" || vm.PID < 1 ||
		vm.PIDStartTime < 1 || vm.HostBootID == "" || inventory.Inventory.SourceDisposition != "RUNNING" ||
		!slices.ContainsFunc(inventory.Inventory.Devices, func(device rt.NativeDevice) bool { return device.VirtioType == 2 }) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "new execution lacks verified running block-root inventory"})
	}
	profile := ""
	if inventory.ManagedStandby() {
		profile = j.Binding.RuntimeBundleID + "/" + inventory.Inventory.Protocol
	}
	observation := nativeObservation(j.Binding, ref, registry.PowerRunning, profile)
	if storage := inventory.ForkStorage(); storage != nil {
		observation.Native.ForkCapabilities = observedForkCapabilities(inventory)
		observation.Native.ForkStorage = storage
	}
	observation.Native.VerifiedLaunch = true
	observation.Native.StopBarrier = &inventory.Inventory.StopBarrier
	observation.RuntimeIdentity = JSON(rt.Identity{BootID: JSON(vm.HostBootID), PID: JSON(vm.PID), StartTime: JSON(vm.PIDStartTime)})
	return j.Done(ctx, registry.TransitionRequest{
		Observation: &observation, Effect: registry.EffectApplied, PromoteBinding: j.Op.Kind == "create", DesiredPower: "running",
		Result: JSON(map[string]any{"machine_id": j.Op.MachineID, "boot_mode": rt.BootSystem, "execution_epoch": ref.ExecutionEpoch}),
	})
}
