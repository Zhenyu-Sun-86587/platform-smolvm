package nodeclient

import (
	"context"
	"encoding/json"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	wc "sandbox-platform/internal/runtime/workerclient"
	"slices"
)

func workerCall[In, Out any](ctx context.Context, a *Adapter, path string, in In) (Out, error) {
	reply, err := call[In, nodeproto.WorkerResult[Out]](ctx, a, nodeproto.WorkerPath+path, in)
	if err == nil && reply.Fault != nil {
		err = reply.Fault
	}
	return reply.Value, err
}
func (a *Adapter) Lifecycle(ctx context.Context, method, key string, expected wc.Execution, payload, into any) error {
	if !slices.Contains(nodeproto.LifecycleActions, method) {
		return rt.ErrUnsupported
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	raw, err := workerCall[nodeproto.PolicyRequest, json.RawMessage](ctx, a, "policy/"+method, nodeproto.PolicyRequest{Expected: expected, OperationID: key, Payload: data})
	if err != nil || into == nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
func (a *Adapter) LookupAttachment(ctx context.Context, expected wc.Execution) (wc.NetworkAttachment, error) {
	return workerCall[wc.Execution, wc.NetworkAttachment](ctx, a, "attachment", expected)
}
func (a *Adapter) LookupPacketWakeEndpoint(ctx context.Context, expected wc.Execution) (wc.PacketWakeEndpoint, error) {
	return workerCall[wc.Execution, wc.PacketWakeEndpoint](ctx, a, "packet-endpoint", expected)
}
func (a *Adapter) PacketWakeRule(ctx context.Context, expected wc.Execution, rule wc.PacketWakeRule, id, digest string) (json.RawMessage, error) {
	return workerCall[nodeproto.PacketRule, json.RawMessage](ctx, a, "packet-rule", nodeproto.PacketRule{Expected: expected, Rule: rule, OperationID: id, Digest: digest})
}
func (a *Adapter) ComputeResources(ctx context.Context, expected wc.Execution) (wc.ComputeResources, error) {
	return workerCall[wc.Execution, wc.ComputeResources](ctx, a, "resources", expected)
}
func (a *Adapter) ManagedCPULimit(ctx context.Context, expected wc.Execution) (wc.CPULimitReceipt, error) {
	return workerCall[wc.Execution, wc.CPULimitReceipt](ctx, a, "cpu-limit", expected)
}
func (a *Adapter) ResizeResources(ctx context.Context, id string, expected wc.Execution, change wc.ResourceChange) (wc.ResizeReceipt, error) {
	return workerCall[nodeproto.ResourceChange, wc.ResizeReceipt](ctx, a, "resize", nodeproto.ResourceChange{Expected: expected, OperationID: id, Change: change})
}
func (a *Adapter) SetManagedCPULimit(ctx context.Context, id string, expected wc.Execution, limit *uint32) (wc.CPULimitReceipt, error) {
	return workerCall[nodeproto.CPULimit, wc.CPULimitReceipt](ctx, a, "set-cpu-limit", nodeproto.CPULimit{Expected: expected, OperationID: id, Millicpus: limit})
}
func (a *Adapter) StatePointCapture(ctx context.Context, expected wc.Execution, id, digest, capture string) (json.RawMessage, error) {
	return workerCall[nodeproto.CaptureRef, json.RawMessage](ctx, a, "statepoint", nodeproto.CaptureRef{Expected: expected, OperationID: id, Digest: digest, CaptureID: capture})
}
func (a *Adapter) ReleaseStatePoint(ctx context.Context, capture, digest string) error {
	_, err := workerCall[nodeproto.ReleaseCapture, nodeproto.Empty](ctx, a, "release-statepoint", nodeproto.ReleaseCapture{CaptureID: capture, Digest: digest})
	return err
}
