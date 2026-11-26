package nodegateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	wc "sandbox-platform/internal/runtime/workerclient"
)

func workerResult[T any](v T, err error) (nodeproto.WorkerResult[T], error) {
	var f *wc.Fault
	if errors.As(err, &f) {
		return nodeproto.WorkerResult[T]{Fault: f}, nil
	}
	return nodeproto.WorkerResult[T]{Value: v}, err
}

func (s *Server) registerWorkerRPC(mux *http.ServeMux) {
	s.registerSample(mux)
	rpc(s, mux, nodeproto.WorkerPath+"resolve-network", func(ctx context.Context, r Route, in *rt.LaunchNetwork) (*rt.LaunchNetwork, error) {
		if in != nil && (in.RouteDomainID != "" || in.CIDR != "") {
			return nil, rt.ErrInvalid
		}
		if r.ResolveNetwork == nil {
			if in != nil {
				return nil, rt.ErrUnsupported
			}
			return nil, nil
		}
		return r.ResolveNetwork(in)
	})
	for _, method := range nodeproto.LifecycleActions {
		rpc(s, mux, nodeproto.WorkerPath+"policy/"+method, func(ctx context.Context, r Route, in nodeproto.PolicyRequest) (nodeproto.WorkerResult[json.RawMessage], error) {
			var out json.RawMessage
			local, ok := r.Adapter.(interface{ LifecycleWorkerSocket() string })
			if !ok || local.LifecycleWorkerSocket() == "" || local.LifecycleWorkerSocket() != r.Worker.Socket {
				return workerResult(out, rt.ErrUnsupported)
			}
			read := method == "GetLifecycleStatus" || method == "QueryLifecycleEvents" || method == "QueryHostActivity" || method == "QueryProtection"
			if !nodeproto.ValidID(in.Expected.InstanceID) || in.Expected.Epoch < 0 || !read && !nodeproto.ValidID(in.OperationID) || len(in.Payload) == 0 || in.Payload[0] != '{' {
				return workerResult(out, rt.ErrInvalid)
			}
			if method == "RequestWake" || method == "AuthorizeWakeActivation" || method == "AcquireRunningLease" {
				if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
					return workerResult(out, err)
				}
			}
			err := r.Worker.Lifecycle(ctx, method, in.OperationID, in.Expected, in.Payload, &out)
			return workerResult(out, err)
		})
	}
	rpc(s, mux, nodeproto.WorkerPath+"attachment", func(ctx context.Context, r Route, in wc.Execution) (nodeproto.WorkerResult[wc.NetworkAttachment], error) {

		return workerResult(r.Worker.LookupAttachment(ctx, in))
	})
	rpc(s, mux, nodeproto.WorkerPath+"packet-endpoint", func(ctx context.Context, r Route, in wc.Execution) (nodeproto.WorkerResult[wc.PacketWakeEndpoint], error) {

		return workerResult(r.Worker.LookupPacketWakeEndpoint(ctx, in))
	})
	rpc(s, mux, nodeproto.WorkerPath+"packet-rule", func(ctx context.Context, r Route, in nodeproto.PacketRule) (nodeproto.WorkerResult[json.RawMessage], error) {
		if in.Rule.Enabled != nil && *in.Rule.Enabled {
			if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
				return workerResult(json.RawMessage(nil), err)
			}
		}
		return workerResult(r.Worker.PacketWakeRule(ctx, in.Expected, in.Rule, in.OperationID, in.Digest))
	})
	rpc(s, mux, nodeproto.WorkerPath+"resources", func(ctx context.Context, r Route, in wc.Execution) (nodeproto.WorkerResult[wc.ComputeResources], error) {

		return workerResult(r.Worker.ComputeResources(ctx, in))
	})
	rpc(s, mux, nodeproto.WorkerPath+"cpu-limit", func(ctx context.Context, r Route, in wc.Execution) (nodeproto.WorkerResult[wc.CPULimitReceipt], error) {

		return workerResult(r.Worker.ManagedCPULimit(ctx, in))
	})
	rpc(s, mux, nodeproto.WorkerPath+"resize", func(ctx context.Context, r Route, in nodeproto.ResourceChange) (nodeproto.WorkerResult[wc.ResizeReceipt], error) {

		return workerResult(r.Worker.ResizeResources(ctx, in.OperationID, in.Expected, in.Change))
	})
	rpc(s, mux, nodeproto.WorkerPath+"set-cpu-limit", func(ctx context.Context, r Route, in nodeproto.CPULimit) (nodeproto.WorkerResult[wc.CPULimitReceipt], error) {

		return workerResult(r.Worker.SetManagedCPULimit(ctx, in.OperationID, in.Expected, in.Millicpus))
	})
	rpc(s, mux, nodeproto.WorkerPath+"statepoint", func(ctx context.Context, r Route, in nodeproto.CaptureRef) (nodeproto.WorkerResult[json.RawMessage], error) {

		return workerResult(r.Worker.StatePointCapture(ctx, in.Expected, in.OperationID, in.Digest, in.CaptureID))
	})
	rpc(s, mux, nodeproto.WorkerPath+"release-statepoint", func(ctx context.Context, r Route, in nodeproto.ReleaseCapture) (nodeproto.WorkerResult[nodeproto.Empty], error) {
		if !nodeproto.ValidID(in.CaptureID) || !nodeproto.ValidDigest(in.Digest) {
			return workerResult(nodeproto.Empty{}, rt.ErrInvalid)
		}
		return workerResult(nodeproto.Empty{}, r.Worker.ReleaseStatePoint(ctx, in.CaptureID, in.Digest))
	})
}
