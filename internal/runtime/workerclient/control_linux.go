package workerclient

import (
	"context"
	"encoding/json"
	"net"

	rt "sandbox-platform/internal/runtime"
)

// BindingControl exposes only the operations used for an already resolved
// binding. Descriptor and peer-identity checks belong to the local Worker.
type BindingControl interface {
	LookupAttachment(context.Context, Execution) (NetworkAttachment, error)
	DialAttachment(context.Context, NetworkAttachment, uint16, string) (net.Conn, error)
	LookupPacketWakeEndpoint(context.Context, Execution) (PacketWakeEndpoint, error)
	PacketWakeRule(context.Context, Execution, PacketWakeRule, string, string) (json.RawMessage, error)
	ManagedCPULimit(context.Context, Execution) (CPULimitReceipt, error)
	SetManagedCPULimit(context.Context, string, Execution, *uint32) (CPULimitReceipt, error)
	ComputeResources(context.Context, Execution) (ComputeResources, error)
	ResizeResources(context.Context, string, Execution, ResourceChange) (ResizeReceipt, error)
	StatePointCapture(context.Context, Execution, string, string, string) (json.RawMessage, error)
	ReleaseStatePoint(context.Context, string, string, string) error
}

func ForAdapter(adapter rt.Adapter, bundle string) BindingControl {
	if remote, ok := adapter.(BindingControl); ok {
		return remote
	}
	if local, ok := adapter.(interface{ WorkerSocket() string }); ok && local.WorkerSocket() != "" && bundle != "" {
		return Client{Socket: local.WorkerSocket(), Bundle: bundle}
	}
	return nil
}

func (c Client) ResizeResources(ctx context.Context, id string, ref Execution, change ResourceChange) (ResizeReceipt, error) {
	return c.Resize(ctx, id, ref, change)
}
