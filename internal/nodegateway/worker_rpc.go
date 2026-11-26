package nodegateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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

func requireWorkerExecution(ctx context.Context, scope nodeproto.Route, route Route, exec wc.Execution) error {
	if !nodeproto.ValidID(exec.InstanceID) || exec.Epoch < 0 ||
		exec.IncarnationID != "" && (exec.Epoch == 0 || !nodeproto.ValidID(exec.IncarnationID)) {
		return rt.ErrInvalid
	}
	if !nodeproto.OwnsManagedInstance(scope.LocalInstanceID, exec.InstanceID) {
		return &nodeproto.Failure{Code: nodeproto.CodeRoute}
	}
	if route.Worker.Socket == "" {
		return rt.ErrUnsupported
	}
	// WorkerResource/1 scopes a binding by route=deep and Bundle. Only the
	// Node additionally knows the exact private Serve namespace. The read-only
	// binding query preserves stopped scopes and never registers a missing
	// Bundle. The subsequent Worker method checks the original expected
	// execution and retains its own stale-identity and idempotent-retry rules.
	_, err := route.Worker.QueryInstanceBinding(ctx, exec.InstanceID)
	return err
}

func workerExecutionRPC[In, Out any](s *Server, mux *http.ServeMux, path string, expected func(In) wc.Execution,
	call func(context.Context, nodeproto.Route, Route, In) (Out, error)) {
	serverRPC(s, mux, nodeproto.WorkerPath+path, s.control, 30*time.Second, true, func(ctx context.Context, scope nodeproto.Route, route Route, in In) (nodeproto.WorkerResult[Out], error) {
		if err := requireWorkerExecution(ctx, scope, route, expected(in)); err != nil {
			var zero Out
			return workerResult(zero, err)
		}
		return workerResult(call(ctx, scope, route, in))
	})
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
		workerExecutionRPC(s, mux, "policy/"+method, func(in nodeproto.PolicyRequest) wc.Execution { return in.Expected },
			func(ctx context.Context, _ nodeproto.Route, r Route, in nodeproto.PolicyRequest) (json.RawMessage, error) {
				var out json.RawMessage
				local, ok := r.Adapter.(interface{ LifecycleWorkerSocket() string })
				if !ok || local.LifecycleWorkerSocket() == "" || local.LifecycleWorkerSocket() != r.Worker.Socket {
					return nil, rt.ErrUnsupported
				}
				read := method == "GetLifecycleStatus" || method == "QueryLifecycleEvents" || method == "QueryHostActivity" || method == "QueryProtection"
				if !read && !nodeproto.ValidID(in.OperationID) || len(in.Payload) == 0 || in.Payload[0] != '{' {
					return nil, rt.ErrInvalid
				}
				if method == "RequestWake" || method == "AuthorizeWakeActivation" || method == "AcquireRunningLease" {
					if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
						return nil, err
					}
				}
				err := r.Worker.Lifecycle(ctx, method, in.OperationID, in.Expected, in.Payload, &out)
				return out, err
			})
	}
	workerExecutionRPC(s, mux, "attachment", func(in wc.Execution) wc.Execution { return in },
		func(ctx context.Context, _ nodeproto.Route, r Route, in wc.Execution) (wc.NetworkAttachment, error) {
			return r.Worker.LookupAttachment(ctx, in)
		})
	workerExecutionRPC(s, mux, "packet-endpoint", func(in wc.Execution) wc.Execution { return in },
		func(ctx context.Context, _ nodeproto.Route, r Route, in wc.Execution) (wc.PacketWakeEndpoint, error) {
			return r.Worker.LookupPacketWakeEndpoint(ctx, in)
		})
	workerExecutionRPC(s, mux, "packet-rule", func(in nodeproto.PacketRule) wc.Execution { return in.Expected },
		func(ctx context.Context, scope nodeproto.Route, r Route, in nodeproto.PacketRule) (json.RawMessage, error) {
			if in.Rule.Enabled != nil && *in.Rule.Enabled {
				if in.Rule.SourceExecution == nil {
					return nil, rt.ErrInvalid
				}
				if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
					return nil, err
				}
			}
			if source := in.Rule.SourceExecution; source != nil {
				if err := requireWorkerExecution(ctx, scope, r, *source); err != nil {
					return nil, err
				}
			}
			return r.Worker.PacketWakeRule(ctx, in.Expected, in.Rule, in.OperationID, in.Digest)
		})
	workerExecutionRPC(s, mux, "resources", func(in wc.Execution) wc.Execution { return in },
		func(ctx context.Context, _ nodeproto.Route, r Route, in wc.Execution) (wc.ComputeResources, error) {
			return r.Worker.ComputeResources(ctx, in)
		})
	workerExecutionRPC(s, mux, "cpu-limit", func(in wc.Execution) wc.Execution { return in },
		func(ctx context.Context, _ nodeproto.Route, r Route, in wc.Execution) (wc.CPULimitReceipt, error) {
			return r.Worker.ManagedCPULimit(ctx, in)
		})
	workerExecutionRPC(s, mux, "resize", func(in nodeproto.ResourceChange) wc.Execution { return in.Expected },
		func(ctx context.Context, _ nodeproto.Route, r Route, in nodeproto.ResourceChange) (wc.ResizeReceipt, error) {
			if !nodeproto.ValidID(in.OperationID) {
				return wc.ResizeReceipt{}, rt.ErrInvalid
			}
			return r.Worker.ResizeResources(ctx, in.OperationID, in.Expected, in.Change)
		})
	workerExecutionRPC(s, mux, "set-cpu-limit", func(in nodeproto.CPULimit) wc.Execution { return in.Expected },
		func(ctx context.Context, _ nodeproto.Route, r Route, in nodeproto.CPULimit) (wc.CPULimitReceipt, error) {
			if !nodeproto.ValidID(in.OperationID) {
				return wc.CPULimitReceipt{}, rt.ErrInvalid
			}
			return r.Worker.SetManagedCPULimit(ctx, in.OperationID, in.Expected, in.Millicpus)
		})
	workerExecutionRPC(s, mux, "statepoint", func(in nodeproto.CaptureRef) wc.Execution { return in.Expected },
		func(ctx context.Context, _ nodeproto.Route, r Route, in nodeproto.CaptureRef) (json.RawMessage, error) {
			if !nodeproto.ValidID(in.OperationID) || !nodeproto.ValidID(in.CaptureID) || !nodeproto.ValidDigest(in.Digest) {
				return nil, rt.ErrInvalid
			}
			return r.Worker.StatePointCapture(ctx, in.Expected, in.OperationID, in.Digest, in.CaptureID)
		})
	scopedRPC(s, mux, nodeproto.WorkerPath+"release-statepoint", func(ctx context.Context, scope nodeproto.Route, r Route, in nodeproto.ReleaseCapture) (nodeproto.WorkerResult[nodeproto.Empty], error) {
		var out nodeproto.Empty
		if !nodeproto.ValidID(in.OperationID) || !nodeproto.ValidID(in.CaptureID) || !nodeproto.ValidDigest(in.Digest) {
			return workerResult(out, rt.ErrInvalid)
		}
		receipt, err := r.Worker.QueryOperation(ctx, in.OperationID)
		if err != nil {
			return workerResult(out, err)
		}
		if !nodeproto.OwnsManagedInstance(scope.LocalInstanceID, receipt.InstanceID) {
			return workerResult(out, &nodeproto.Failure{Code: nodeproto.CodeRoute})
		}
		point := receipt.StatePoint
		if receipt.Kind != rt.VMSnapshot || receipt.Phase != "COMPLETED" || point == nil ||
			point.CaptureID != in.CaptureID || point.ManifestDigest != in.Digest {
			return workerResult(out, rt.ErrConflict)
		}
		return workerResult(out, r.Worker.ReleaseStatePoint(ctx, in.CaptureID, in.Digest, in.OperationID))
	})
}
