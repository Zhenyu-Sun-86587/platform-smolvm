package nodeclient

import (
	"context"
	"io"
	"slices"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

// Adapter addresses one registered runtime route on one enrolled Worker.
type Adapter struct {
	rt.UnsupportedOperations
	client *Client
	scope  nodeproto.Route
}

var (
	_ rt.Adapter            = (*Adapter)(nil)
	_ rt.NativeStopControl  = (*Adapter)(nil)
	_ rt.NativeLauncher     = (*Adapter)(nil)
	_ rt.NativeForkControl  = (*Adapter)(nil)
	_ rt.ForkRetryControl   = (*Adapter)(nil)
	_ rt.ForkCancelControl  = (*Adapter)(nil)
	_ rt.StatePointControl  = (*Adapter)(nil)
	_ rt.NativeGuestControl = (*Adapter)(nil)
)

func (c *Client) Adapter(route nodeproto.Route) *Adapter {
	return &Adapter{client: c, scope: route}
}

func (a *Adapter) Route() nodeproto.Route { return a.scope }

func (a *Adapter) routeInfo() nodeproto.RouteInfo {
	a.client.mu.RLock()
	defer a.client.mu.RUnlock()
	return a.client.routeInfo[a.scope]
}

func (a *Adapter) NativeStopContract() bool { return a.routeInfo().NativeStop }

func (a *Adapter) NativeStandbyContract() bool { return a.routeInfo().NativeStandby }

func (a *Adapter) LifecyclePolicyContract() bool { return a.routeInfo().NativeStop }

func (a *Adapter) Admit(action string) error {
	if slices.Contains(a.routeInfo().AdmittedActions, action) {
		return nil
	}
	return rt.ErrUnsupported
}

func (a *Adapter) MachineMetrics(ctx context.Context, name string, execution rt.ExecutionRef) (rt.Metrics, error) {
	return call[nodeproto.MachineMetrics, rt.Metrics](ctx, a, nodeproto.MachineMetricsPath, nodeproto.MachineMetrics{Name: name, Execution: execution})
}

func (a *Adapter) Health(ctx context.Context) (rt.Health, error) {
	return call[nodeproto.Empty, rt.Health](ctx, a, nodeproto.HealthPath, nodeproto.Empty{})
}

func (a *Adapter) Ready(ctx context.Context) error {
	_, err := call[nodeproto.Empty, nodeproto.Empty](ctx, a, nodeproto.ReadyPath, nodeproto.Empty{})
	return err
}

func (a *Adapter) Observe(ctx context.Context, name string) (rt.Observation, error) {
	return call[nodeproto.Name, rt.Observation](ctx, a, nodeproto.ObservePath, nodeproto.Name{Name: name})
}

func (a *Adapter) Capacity(ctx context.Context) (rt.Capacity, error) {
	return call[nodeproto.Empty, rt.Capacity](ctx, a, nodeproto.CapacityPath, nodeproto.Empty{})
}

func (a *Adapter) DiscoverExecution(ctx context.Context, name string) (rt.ExecutionRef, error) {
	return call[nodeproto.Name, rt.ExecutionRef](ctx, a, nodeproto.DiscoverExecutionPath, nodeproto.Name{Name: name})
}

func (a *Adapter) DiscoverLaunchBinding(ctx context.Context, name string) (rt.LaunchBinding, error) {
	return call[nodeproto.Name, rt.LaunchBinding](ctx, a, nodeproto.DiscoverBindingPath, nodeproto.Name{Name: name})
}

func (a *Adapter) NativeInventory(ctx context.Context, expected rt.ExecutionRef) (rt.NativeInventory, error) {
	inventory, err := call[rt.ExecutionRef, nodeproto.Inventory](ctx, a, nodeproto.InventoryPath, expected)
	inventory.Native.Capabilities = inventory.Capabilities
	return inventory.Native, err
}

func (a *Adapter) QueryOperation(ctx context.Context, id string) (rt.OperationReceipt, error) {
	return call[nodeproto.Operation, rt.OperationReceipt](ctx, a, nodeproto.QueryOperationPath, nodeproto.Operation{OperationID: id})
}

func (a *Adapter) ReconcileKnown(ctx context.Context, in nodeproto.ReconcileRequest) (nodeproto.ReconcileResult, error) {
	return call[nodeproto.ReconcileRequest, nodeproto.ReconcileResult](ctx, a, nodeproto.ReconcileKnownPath, in)
}

func (a *Adapter) SubmitLaunch(ctx context.Context, in rt.LaunchSubmission) (rt.OperationReceipt, error) {
	return call[rt.LaunchSubmission, rt.OperationReceipt](ctx, a, nodeproto.SubmitLaunchPath, in)
}

func (a *Adapter) SubmitNative(ctx context.Context, in rt.NativeSubmission) (rt.OperationReceipt, error) {
	return call[rt.NativeSubmission, rt.OperationReceipt](ctx, a, nodeproto.SubmitPowerPath, in)
}

func (a *Adapter) SubmitFork(ctx context.Context, in rt.ForkSubmission) (rt.OperationReceipt, error) {
	return call[rt.ForkSubmission, rt.OperationReceipt](ctx, a, nodeproto.SubmitForkPath, in)
}

func (a *Adapter) RetryFork(ctx context.Context, id string, in rt.ForkControl) (rt.OperationReceipt, error) {
	return call[nodeproto.ForkControl, rt.OperationReceipt](ctx, a, nodeproto.RetryForkPath, nodeproto.ForkControl{OperationID: id, Control: in})
}

func (a *Adapter) CancelFork(ctx context.Context, id string, in rt.ForkControl) (rt.OperationReceipt, error) {
	return call[nodeproto.ForkControl, rt.OperationReceipt](ctx, a, nodeproto.CancelForkPath, nodeproto.ForkControl{OperationID: id, Control: in})
}

func (a *Adapter) SubmitStatePoint(ctx context.Context, in rt.StatePointSubmission) (rt.OperationReceipt, error) {
	return call[rt.StatePointSubmission, rt.OperationReceipt](ctx, a, nodeproto.SubmitStatePointPath, in)
}

func (a *Adapter) StatePointCapability(ctx context.Context, kind string) (rt.Capability, error) {
	return call[nodeproto.Kind, rt.Capability](ctx, a, nodeproto.StatePointCapPath, nodeproto.Kind{Kind: kind})
}

func (a *Adapter) SystemBoot(ctx context.Context) (rt.Capability, error) {
	profiles, err := a.SystemLaunchCapabilities(ctx)
	return profiles.Boot, err
}

func (a *Adapter) SystemLaunchCapabilities(ctx context.Context) (rt.SystemLaunchProfiles, error) {
	out, err := call[nodeproto.Empty, nodeproto.SystemLaunchCapabilities](ctx, a, nodeproto.BootCapabilityPath, nodeproto.Empty{})
	unknown := rt.FullStateCapabilities(false)[rt.KernelCowLaunchCapability]
	if err != nil {
		return rt.SystemLaunchProfiles{Boot: unknown, Growth: unknown, KernelCow: unknown}, err
	}
	if out.KernelCow.Status == "" {
		out.KernelCow = unknown
	}
	return rt.SystemLaunchProfiles{Boot: out.Boot, Growth: out.Growth, KernelCow: out.KernelCow}, nil
}

func (a *Adapter) GuestPing(ctx context.Context, ref rt.ExecutionRef) error {
	_, err := call[rt.ExecutionRef, nodeproto.Empty](ctx, a, nodeproto.GuestPingPath, ref)
	return err
}

func (a *Adapter) GuestExec(ctx context.Context, ref rt.ExecutionRef, in rt.ExecRequest) (rt.ExecResult, error) {
	return call[nodeproto.GuestExec, rt.ExecResult](ctx, a, nodeproto.GuestExecPath, nodeproto.GuestExec{Expected: ref, Exec: in})
}

func (a *Adapter) GuestUpload(ctx context.Context, ref rt.ExecutionRef, path string, source io.Reader, options rt.FileOptions) error {
	if options.Size > rt.PrivateGuestUploadLimit {
		return rt.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(source, rt.PrivateGuestUploadLimit+1))
	if err != nil {
		return err
	}
	if len(data) > rt.PrivateGuestUploadLimit || (options.Size >= 0 && int64(len(data)) != options.Size) {
		return rt.ErrInvalid
	}
	_, err = call[nodeproto.GuestUpload, nodeproto.Empty](ctx, a, nodeproto.GuestUploadPath,
		nodeproto.GuestUpload{Expected: ref, Path: path, Options: options, Contents: data})
	return err
}
