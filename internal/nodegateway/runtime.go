package nodegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

func require[T any](route Route) (T, error) {
	a, ok := route.Adapter.(T)
	if !ok {
		return a, rt.ErrUnsupported
	}
	return a, nil
}

func (s *Server) registerRuntimeRPC(mux *http.ServeMux) {
	rpc(s, mux, nodeproto.HealthPath, func(ctx context.Context, route Route, _ nodeproto.Empty) (rt.Health, error) {
		return route.Adapter.Health(ctx)
	})
	rpc(s, mux, nodeproto.ReadyPath, func(ctx context.Context, route Route, _ nodeproto.Empty) (nodeproto.Empty, error) {
		return nodeproto.Empty{}, route.Adapter.Ready(ctx)
	})
	rpc(s, mux, nodeproto.ObservePath, func(ctx context.Context, route Route, in nodeproto.Name) (rt.Observation, error) {
		return route.Adapter.Observe(ctx, in.Name)
	})
	rpc(s, mux, nodeproto.CapacityPath, func(ctx context.Context, route Route, _ nodeproto.Empty) (rt.Capacity, error) {
		return route.Adapter.Capacity(ctx)
	})
	rpc(s, mux, nodeproto.DiscoverExecutionPath, func(ctx context.Context, route Route, in nodeproto.Name) (rt.ExecutionRef, error) {
		if a, err := require[rt.NativeControl](route); err == nil {
			return a.DiscoverExecution(ctx, in.Name)
		}
		return rt.ExecutionRef{}, rt.ErrUnsupported
	})
	rpc(s, mux, nodeproto.DiscoverBindingPath, func(ctx context.Context, route Route, in nodeproto.Name) (rt.LaunchBinding, error) {
		return discoverBinding(ctx, route, in.Name)
	})
	rpc(s, mux, nodeproto.InventoryPath, func(ctx context.Context, route Route, in rt.ExecutionRef) (nodeproto.Inventory, error) {
		a, err := require[rt.NativeControl](route)
		if err != nil {
			return nodeproto.Inventory{}, err
		}
		inventory, err := a.NativeInventory(ctx, in)
		return nodeproto.Inventory{Native: inventory, Capabilities: inventory.Capabilities}, err
	})
	rpc(s, mux, nodeproto.BootCapabilityPath, func(ctx context.Context, route Route, _ nodeproto.Empty) (nodeproto.SystemLaunchCapabilities, error) {
		a, err := require[interface {
			SystemLaunchCapabilities(context.Context) (rt.Capability, rt.Capability, error)
		}](route)
		if err != nil {
			return nodeproto.SystemLaunchCapabilities{}, err
		}
		if len(route.RootGenerations) == 0 {
			unsupported := rt.Capability{Status: "unsupported", ReasonCode: "NODE_ROOT_GENERATION_UNAVAILABLE"}
			return nodeproto.SystemLaunchCapabilities{Boot: unsupported, Growth: unsupported}, nil
		}
		boot, growth, err := a.SystemLaunchCapabilities(ctx)
		return nodeproto.SystemLaunchCapabilities{Boot: boot, Growth: growth}, err
	})
	rpc(s, mux, nodeproto.StatePointCapPath, func(ctx context.Context, route Route, in nodeproto.Kind) (rt.Capability, error) {
		if a, err := require[rt.StatePointControl](route); err == nil {
			return a.StatePointCapability(ctx, in.Kind)
		}
		return rt.Capability{}, rt.ErrUnsupported
	})
	s.registerMutations(mux)
	scopedRPC(s, mux, nodeproto.QueryOperationPath, func(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.Operation) (rt.OperationReceipt, error) {
		return s.queryOperation(ctx, scope, route, in.OperationID)
	})
	scopedRPC(s, mux, nodeproto.ReconcileKnownPath, s.reconcileKnown)
	guestRPC(s, mux, nodeproto.GuestPingPath, func(ctx context.Context, route Route, in rt.ExecutionRef) (nodeproto.Empty, error) {
		if !nodeproto.ValidExecution(in) {
			return nodeproto.Empty{}, rt.ErrInvalid
		}
		if a, err := require[rt.NativeGuestControl](route); err == nil {
			return nodeproto.Empty{}, a.GuestPing(ctx, in)
		}
		return nodeproto.Empty{}, rt.ErrUnsupported
	})
	guestRPC(s, mux, nodeproto.GuestExecPath, func(ctx context.Context, route Route, in nodeproto.GuestExec) (rt.ExecResult, error) {
		if !nodeproto.ValidExecution(in.Expected) {
			return rt.ExecResult{}, rt.ErrInvalid
		}
		if a, err := require[rt.NativeGuestControl](route); err == nil {
			return a.GuestExec(ctx, in.Expected, in.Exec)
		}
		return rt.ExecResult{}, rt.ErrUnsupported
	})
	guestRPC(s, mux, nodeproto.GuestUploadPath, func(ctx context.Context, route Route, in nodeproto.GuestUpload) (nodeproto.Empty, error) {
		if !nodeproto.ValidExecution(in.Expected) || len(in.Contents) > rt.PrivateGuestUploadLimit {
			return nodeproto.Empty{}, rt.ErrInvalid
		}
		if a, err := require[rt.NativeGuestControl](route); err == nil {
			return nodeproto.Empty{}, a.GuestUpload(ctx, in.Expected, in.Path, bytes.NewReader(in.Contents), in.Options)
		}
		return nodeproto.Empty{}, rt.ErrUnsupported
	})
}

func scopedRPC[In, Out any](s *Server, mux *http.ServeMux, path string, f func(context.Context, nodeproto.Route, Route, In) (Out, error)) {
	serverRPC(s, mux, path, s.control, 15*time.Second, true, f)
}

func guestRPC[In, Out any](s *Server, mux *http.ServeMux, path string, f func(context.Context, Route, In) (Out, error)) {
	serverRPC(s, mux, path, s.guest, 130*time.Second, true, func(ctx context.Context, _ nodeproto.Route, route Route, in In) (Out, error) {
		return f(ctx, route, in)
	})
}

func discoverBinding(ctx context.Context, route Route, name string) (rt.LaunchBinding, error) {
	if a, err := require[interface {
		DiscoverLaunchBinding(context.Context, string) (rt.LaunchBinding, error)
	}](route); err == nil {
		return a.DiscoverLaunchBinding(ctx, name)
	}
	return rt.LaunchBinding{}, rt.ErrUnsupported
}

func (s *Server) registerMutations(mux *http.ServeMux) {
	scopedRPC(s, mux, nodeproto.SubmitPowerPath, func(ctx context.Context, scope nodeproto.Route, route Route, in rt.NativeSubmission) (rt.OperationReceipt, error) {
		if !nodeproto.ValidExecution(in.Expected) || rt.NativePowerCapability(in.Kind) == "" {
			return rt.OperationReceipt{}, rt.ErrInvalid
		}
		a, err := require[rt.NativeControl](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		command := nativeCommand(in.Expected, in.Kind, in.RequestDigest)
		return s.submit(ctx, scope, in.OperationID, submitStep, command, in, a, func(ctx context.Context) (rt.OperationReceipt, error) {
			return a.SubmitNative(ctx, in)
		}, false, in.Kind == rt.Wake)
	})
	scopedRPC(s, mux, nodeproto.SubmitLaunchPath, func(ctx context.Context, scope nodeproto.Route, route Route, in rt.LaunchSubmission) (rt.OperationReceipt, error) {
		if err := validateLaunch(route, in); err != nil {
			return rt.OperationReceipt{}, err
		}
		a, err := require[rt.NativeLauncher](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		command := bindingCommand(in.Expected, in.Kind, in.RequestDigest)
		return s.submit(ctx, scope, in.OperationID, submitStep, command, in, a, func(ctx context.Context) (rt.OperationReceipt, error) {
			return a.SubmitLaunch(ctx, in)
		}, in.Expected.ExecutionEpoch == 0, true)
	})
	scopedRPC(s, mux, nodeproto.SubmitStatePointPath, func(ctx context.Context, scope nodeproto.Route, route Route, in rt.StatePointSubmission) (rt.OperationReceipt, error) {
		if !validBinding(in.Expected) || !rt.ManagedStatePoint(in.Kind) || !nodeproto.ValidID(in.Parameters.CaptureID) {
			return rt.OperationReceipt{}, rt.ErrInvalid
		}
		a, err := require[rt.StatePointControl](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		command := bindingCommand(in.Expected, in.Kind, in.RequestDigest)
		return s.submit(ctx, scope, in.OperationID, submitStep, command, in, a, func(ctx context.Context) (rt.OperationReceipt, error) {
			return a.SubmitStatePoint(ctx, in)
		}, false, in.Kind == rt.VMRestore || in.Kind == rt.VMRollback)
	})
	scopedRPC(s, mux, nodeproto.SubmitForkPath, func(ctx context.Context, scope nodeproto.Route, route Route, in rt.ForkSubmission) (rt.OperationReceipt, error) {
		if !nodeproto.ValidExecution(in.Expected) || !rt.IsNativeFork(in.Kind) {
			return rt.OperationReceipt{}, rt.ErrInvalid
		}
		a, err := require[rt.NativeForkControl](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		command := nativeCommand(in.Expected, in.Kind, in.RequestDigest)
		return s.submit(ctx, scope, in.OperationID, submitStep, command, in, a, func(ctx context.Context) (rt.OperationReceipt, error) {
			return a.SubmitFork(ctx, in)
		}, true, true)
	})
	scopedRPC(s, mux, nodeproto.RetryForkPath, func(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.ForkControl) (rt.OperationReceipt, error) {
		a, err := require[rt.ForkRetryControl](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		return s.controlFork(ctx, scope, route, in, "retry", a.RetryFork)
	})
	scopedRPC(s, mux, nodeproto.CancelForkPath, func(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.ForkControl) (rt.OperationReceipt, error) {
		a, err := require[rt.ForkCancelControl](route)
		if err != nil {
			return rt.OperationReceipt{}, err
		}
		return s.controlFork(ctx, scope, route, in, "cancel", a.CancelFork)
	})
}

func validBinding(in rt.LaunchBinding) bool {
	return nodeproto.ValidID(in.InstanceID) && in.ExecutionEpoch >= 0 &&
		(in.IncarnationID == nil || (in.ExecutionEpoch > 0 && nodeproto.ValidID(*in.IncarnationID)))
}

func validateLaunch(route Route, in rt.LaunchSubmission) error {
	p := in.Parameters
	if !validBinding(in.Expected) || in.Expected.IncarnationID != nil || in.Kind != rt.Launch || !nodeproto.ValidID(p.Name) ||
		p.BootProfile != rt.SystemBootProfile || p.Init != "/sbin/init" || p.CPUs == 0 || p.MemoryMiB == 0 || len(p.Disks) == 0 {
		return rt.ErrInvalid
	}
	roots := 0
	for _, disk := range p.Disks {
		if disk.Role == "root" {
			roots++
			if !route.RootGenerations[disk.GenerationID] {
				return rt.ErrUnsupported
			}
		}
		if !nodeproto.ValidID(disk.AttachmentID) || !nodeproto.ValidID(disk.DeviceID) ||
			(disk.GenerationID != "" && !nodeproto.ValidID(disk.GenerationID)) ||
			(disk.VolumeID != "" && !nodeproto.ValidID(disk.VolumeID)) {
			return rt.ErrInvalid
		}
	}
	if roots != 1 {
		return rt.ErrInvalid
	}
	if route.ResolveNetwork == nil {
		if p.Network != nil {
			return rt.ErrUnsupported
		}
		return nil
	}
	resolved, err := route.ResolveNetwork(p.Network)
	if err != nil {
		return err
	}
	if (resolved == nil) != (p.Network == nil) || (resolved != nil && *resolved != *p.Network) {
		return rt.ErrInvalid
	}
	return nil
}

func bindingCommand(binding rt.LaunchBinding, kind, digest string) commandScope {
	return commandScope{InstanceID: binding.InstanceID, Incarnation: binding.IncarnationID, Epoch: binding.ExecutionEpoch, Kind: kind, Native: digest}
}

func nativeCommand(ref rt.ExecutionRef, kind, digest string) commandScope {
	return bindingCommand(rt.LaunchBinding{InstanceID: ref.InstanceID, IncarnationID: &ref.IncarnationID, ExecutionEpoch: ref.ExecutionEpoch}, kind, digest)
}

func uncertain() error { return &nodeproto.Failure{Code: nodeproto.CodeUnknown} }

func (s *Server) submit(ctx context.Context, route nodeproto.Route, id, step string, scope commandScope, in any, backend rt.NativeControl,
	send func(context.Context) (rt.OperationReceipt, error), activeOnly, blockedByDrain bool) (receipt rt.OperationReceipt, err error) {
	if !nodeproto.ValidID(id) || !nodeproto.ValidDigest(scope.Native) {
		return receipt, rt.ErrInvalid
	}
	scope.Route = route
	raw, err := json.Marshal(in)
	if err != nil {
		return receipt, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	entry, first, err := s.journal.begin(ctx, id, step, scope, digest, activeOnly, blockedByDrain)
	if err != nil {
		return receipt, err
	}
	if entry.Receipt != nil {
		return *entry.Receipt, nil
	}
	if entry.Refusal != "" {
		return receipt, nativeRefused(entry.Refusal)
	}
	if !first {
		if step != submitStep {
			// Native QueryOperation exposes the current fork result but does not
			// identify the individual control request that advanced its revision.
			return receipt, uncertain()
		}
		receipt, err = backend.QueryOperation(ctx, id)
		if err != nil || !scope.verifies(id, receipt) {
			return rt.OperationReceipt{}, uncertain()
		}
		if err := s.journal.remember(ctx, id, step, receipt); err != nil {
			return rt.OperationReceipt{}, uncertain()
		}
		return receipt, nil
	}

	// The durable record is the send fence. A client disconnect cannot stop
	// an accepted operation or allow either side to send it a second time.
	dispatch, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	defer cancel()
	receipt, err = send(dispatch)
	if (err != nil || !scope.verifies(id, receipt)) && step == submitStep {
		var fault *rt.Error
		confirmedRefusal := errors.As(err, &fault) && nodeproto.NativeRefusal(fault.NativeCode)
		receipt, err = backend.QueryOperation(dispatch, id)
		if confirmedRefusal && nativeNotFound(err) {
			persist, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer finish()
			if err := s.journal.refuse(persist, id, step, fault.NativeCode); err != nil {
				return rt.OperationReceipt{}, uncertain()
			}
			return rt.OperationReceipt{}, nativeRefused(fault.NativeCode)
		}
	}
	if err != nil || !scope.verifies(id, receipt) {
		return rt.OperationReceipt{}, uncertain()
	}
	persist, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer finish()
	if err := s.journal.remember(persist, id, step, receipt); err != nil {
		return rt.OperationReceipt{}, uncertain()
	}
	return receipt, nil
}

func (s *Server) controlFork(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.ForkControl, action string,
	send func(context.Context, string, rt.ForkControl) (rt.OperationReceipt, error)) (rt.OperationReceipt, error) {
	if !nodeproto.ValidID(in.OperationID) || !nodeproto.ValidExecution(in.Control.Expected) || in.Control.ExpectedRevision < 1 {
		return rt.OperationReceipt{}, rt.ErrInvalid
	}
	a, err := require[rt.NativeControl](route)
	if err != nil {
		return rt.OperationReceipt{}, err
	}
	original, err := s.queryOperation(ctx, scope, route, in.OperationID)
	if err != nil {
		return rt.OperationReceipt{}, err
	}
	command := nativeCommand(in.Control.Expected, original.Kind, in.Control.RequestDigest)
	command.Route = scope
	if !rt.IsNativeFork(original.Kind) || !command.verifies(in.OperationID, original) {
		return rt.OperationReceipt{}, rt.ErrConflict
	}
	return s.submit(ctx, scope, in.OperationID, action+":"+strconv.FormatInt(in.Control.ExpectedRevision, 10),
		command, in, a, func(ctx context.Context) (rt.OperationReceipt, error) {
			return send(ctx, in.OperationID, in.Control)
		}, action == "retry", action == "retry")
}

func (s *Server) queryOperation(ctx context.Context, scope nodeproto.Route, route Route, id string) (rt.OperationReceipt, error) {
	if !nodeproto.ValidID(id) {
		return rt.OperationReceipt{}, rt.ErrInvalid
	}
	a, err := require[rt.NativeControl](route)
	if err != nil {
		return rt.OperationReceipt{}, err
	}
	entry, err := journalLookup(ctx, s.journal.db, id, submitStep)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rt.OperationReceipt{}, err
	}
	retained := err == nil
	if retained && entry.Scope.Route != scope {
		return rt.OperationReceipt{}, &nodeproto.Failure{Code: nodeproto.CodeRoute}
	}
	receipt, err := a.QueryOperation(ctx, id)
	if retained {
		if entry.Refusal != "" && nativeNotFound(err) {
			return rt.OperationReceipt{}, &nodeproto.Failure{Code: nodeproto.CodeNotFound}
		}
		if err != nil || !entry.Scope.verifies(id, receipt) {
			return rt.OperationReceipt{}, uncertain()
		}
		if err := s.journal.remember(ctx, id, submitStep, receipt); err != nil {
			return rt.OperationReceipt{}, uncertain()
		}
	}
	if !retained && errors.Is(err, rt.ErrNotFound) && !nativeNotFound(err) {
		return rt.OperationReceipt{}, uncertain()
	}
	return receipt, err
}

func nativeNotFound(err error) bool {
	var fault *rt.Error
	return errors.As(err, &fault) && errors.Is(fault, rt.ErrNotFound) && fault.NativeCode == "E_NOT_FOUND"
}

func nativeRefused(code string) error {
	return &nodeproto.Failure{Code: nodeproto.CodeNativeRefused, NativeCode: code}
}

func (s *Server) reconcileKnown(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.ReconcileRequest) (nodeproto.ReconcileResult, error) {
	if len(in.Bindings)+len(in.Operations) > nodeproto.ReconcileLimit {
		return nodeproto.ReconcileResult{}, rt.ErrInvalid
	}
	ids, err := s.journal.operationIDs(ctx, scope, nodeproto.ReconcileLimit+1)
	if err != nil {
		return nodeproto.ReconcileResult{}, err
	}
	ids = append(append([]string{}, in.Operations...), ids...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	limit := nodeproto.ReconcileLimit - len(in.Bindings)
	out := nodeproto.ReconcileResult{Coverage: nodeproto.KnownOnly, JournalLimited: len(ids) > limit,
		Operations: []nodeproto.KnownOperation{}, Bindings: []nodeproto.KnownBinding{}}
	// Central-supplied IDs take priority over journal discoveries.
	seen := make(map[string]bool, limit)
	for _, id := range append(append([]string{}, in.Operations...), ids...) {
		if seen[id] || len(out.Operations) == limit {
			continue
		}
		if ctx.Err() != nil {
			return nodeproto.ReconcileResult{}, ctx.Err()
		}
		seen[id] = true
		receipt, err := s.queryOperation(ctx, scope, route, id)
		item := nodeproto.KnownOperation{OperationID: id}
		if err != nil {
			item.Issue = nodeproto.FailureCode(err)
		} else {
			item.Receipt = &receipt
		}
		out.Operations = append(out.Operations, item)
	}
	slices.SortFunc(out.Operations, func(a, b nodeproto.KnownOperation) int { return strings.Compare(a.OperationID, b.OperationID) })
	for _, name := range in.Bindings {
		if ctx.Err() != nil {
			return nodeproto.ReconcileResult{}, ctx.Err()
		}
		binding, err := discoverBinding(ctx, route, name)
		item := nodeproto.KnownBinding{Name: name}
		if err != nil {
			item.Issue = nodeproto.FailureCode(err)
		} else {
			item.Binding = &binding
		}
		out.Bindings = append(out.Bindings, item)
	}
	return out, nil
}
