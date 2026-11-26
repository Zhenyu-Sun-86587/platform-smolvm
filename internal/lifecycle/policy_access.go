package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"time"
)

type AccessEndpoint struct {
	Trigger     string
	Revision    int64
	Reauthorize func(context.Context) error
}
type protectionReceipt struct {
	ID          string `json:"protection_id"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Revision    int64  `json:"revision"`
	Wake        string `json:"wake_operation"`
	Incarnation string `json:"incarnation_id"`
}

func (e *Engine) checkAccessPrincipal(ctx context.Context, p auth.Principal, machine string) error {
	var scope string
	err := e.DB.ReadDB().QueryRowContext(ctx, `SELECT c.scope_json FROM credentials c JOIN principals p ON p.id=c.principal_id
        WHERE c.id=? AND p.id=? AND p.disabled=0 AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>?)`,
		p.CredentialID, p.ID, time.Now().UnixMilli()).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	var actual auth.Scope
	if err = json.Unmarshal([]byte(scope), &actual); err != nil {
		return err
	}
	if p.Scope != actual || actual.MachineID != "" && actual.MachineID != machine {
		return auth.ErrForbidden
	}
	return nil
}

func (e *Engine) workerAccess(parent context.Context, p auth.Principal, b registry.Binding, client workerclient.Client, entry AccessEndpoint) (registry.Binding, func(), error) {
	var none registry.Binding
	ctx, cancel := context.WithTimeout(parent, time.Duration(e.Config.Gateway.WaitSeconds)*time.Second)
	defer cancel()
	id, err := registry.NewID("access-")
	if err != nil {
		return none, nil, err
	}
	ack := protectionReceipt{}
	scope := WorkerExecution(b)
	enteredStopped := b.PowerState == registry.PowerStopped
	assigned := false
	request := func(ctx context.Context, method, key string, args map[string]any, out *protectionReceipt) error {
		args["principal"] = p.ID
		args["protection_id"] = id
		return client.Lifecycle(ctx, method, key, scope, args, out)
	}
	read := func(ctx context.Context) (protectionReceipt, error) {
		var current protectionReceipt
		err := request(ctx, "QueryProtection", "", map[string]any{}, &current)
		return current, err
	}
	var release func()
	defer func() {
		if !assigned && release != nil {
			release()
		}
	}()
	for {
		m, err := e.DB.GetMachine(ctx, b.MachineID)
		if err != nil {
			return none, nil, err
		}
		if m.CurrentBinding == nil || m.CurrentBinding.ID != b.ID || m.DeletedAt != nil {
			return none, nil, registry.ErrGeneration
		}
		b = *m.CurrentBinding
		st, err := e.ObservePolicy(ctx, b, client)
		if err != nil {
			return none, nil, err
		}
		scope = st.Execution()
		if ack.ID == "" {
			if err = e.checkAccessPrincipal(ctx, p, m.ID); err != nil {
				return none, nil, err
			}
			if entry.Reauthorize != nil {
				if err = entry.Reauthorize(ctx); err != nil {
					return none, nil, err
				}
			}
			if st.Power == "STOPPED" {
				if !enteredStopped {
					return none, nil, &Failure{Code: "STATE_CONFLICT", Message: "execution stopped while access was pending"}
				}
				policy, _ := e.DB.LifecyclePolicy(ctx, m.ID)
				if !policy.Desired.Wake.ColdStart {
					return none, nil, &Failure{Code: "SERVICE_NOT_READY", Message: "cold wake is disabled"}
				}
			}
			// A stable wake demand also covers an observed Running source that may
			// cross a hibernate decision between status and admission.
			if release == nil {
				a, err := e.registerConnection(ctx, b, id, p.ID, scope)
				if err != nil {
					return none, nil, err
				}
				release = e.connectionRelease(a, client)
			}
			err = request(ctx, "RequestWake", id+"-admit", map[string]any{"reason": entry.Trigger, "trigger": entry.Trigger,
				"expected_binding_revision": st.Revision, "authorization_revision": 1, "endpoint_revision": entry.Revision}, &ack)
			if err != nil {
				var fault *workerclient.Fault
				if errors.As(err, &fault) && (fault.Code == "E_STALE_EXECUTION" || fault.Code == "E_STALE_REVISION") {
					continue
				}
				return none, nil, err
			}
		}
		ack, err = read(ctx)
		if err != nil {
			return none, nil, err
		}
		if ack.State == "released" {
			return none, nil, auth.ErrForbidden
		}
		if ack.Wake != "" && ack.Kind == "wake" && st.Power == "RUNNING" {
			allowed := e.checkAccessPrincipal(ctx, p, m.ID)
			if allowed == nil && entry.Reauthorize != nil {
				allowed = entry.Reauthorize(ctx)
			}
			err = request(ctx, "AuthorizeWakeActivation", fmt.Sprintf("%s-confirm-%d", id, ack.Revision), map[string]any{"expected_revision": ack.Revision,
				"wake_operation": ack.Wake, "authorized": allowed == nil, "authorization_revision": 1, "endpoint_revision": entry.Revision}, &ack)
			if allowed != nil {
				return none, nil, allowed
			}
			if err != nil {
				var fault *workerclient.Fault
				if !errors.As(err, &fault) || (fault.Code != "E_STATE_CONFLICT" && fault.Code != "E_STALE_REVISION") {
					return none, nil, err
				}
			}
		}
		if ack.Kind == "hold" && ack.State == "active" && ack.Incarnation == scope.IncarnationID && st.Power == "RUNNING" {
			if b.ExecutionEpoch != scope.Epoch || b.IncarnationID != scope.IncarnationID {
				adapter, err := e.ForBinding(b)
				if err != nil {
					return none, nil, err
				}
				if err = e.projectAutoPower(ctx, b, adapter, st, ack.Wake); err != nil && !errors.Is(err, registry.ErrStaleObservation) {
					return none, nil, err
				}
				fresh, err := e.DB.GetMachine(ctx, m.ID)
				if err != nil {
					return none, nil, err
				}
				if fresh.CurrentBinding == nil || fresh.CurrentBinding.ID != b.ID || fresh.DeletedAt != nil {
					return none, nil, registry.ErrGeneration
				}
				if WorkerExecution(*fresh.CurrentBinding) != scope || fresh.CurrentBinding.PowerState != registry.PowerRunning {
					select {
					case <-ctx.Done():
						return none, nil, ctx.Err()
					case <-time.After(100 * time.Millisecond):
						continue
					}
				}
				b = *fresh.CurrentBinding
			}
			if err = e.checkAccessPrincipal(ctx, p, m.ID); err != nil {
				return none, nil, err
			}
			if entry.Reauthorize != nil {
				if err = entry.Reauthorize(ctx); err != nil {
					return none, nil, err
				}
			}
			assigned = true
			return b, release, nil
		}
		select {
		case <-ctx.Done():
			return none, nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// Confirm a Worker automatic transition with the same native receipt and live
// inventory used by explicit platform operations before updating the binding.
func (e *Engine) projectAutoPower(ctx context.Context, b registry.Binding, adapter rt.Adapter, st WorkerLifecycleStatus, operation string) error {
	control, ok := adapter.(rt.StatePointControl)
	if !ok {
		return rt.ErrUnsupported
	}
	if st.Power == "HIBERNATED" && operation != "" {
		op, err := control.QueryOperation(ctx, operation)
		if err != nil {
			return err
		}
		if op.Phase != "COMPLETED" || op.InstanceID != st.Instance || op.BundleDigest != b.RuntimeBundleID ||
			op.Kind != "hibernate" || op.StatePoint == nil || op.StatePoint.Phase != "hibernated" || !op.StatePoint.SourceReleased ||
			op.ExpectedExecutionEpoch != b.ExecutionEpoch || op.StatePoint.CaptureID != st.StatePoint || !statePointDigestValid(op.StatePoint.ManifestDigest) {
			return registry.ErrStaleObservation
		}
		target, err := control.DiscoverLaunchBinding(ctx, b.RuntimeName)
		if err != nil {
			return err
		}
		if target.IncarnationID != nil || target.InstanceID != st.Instance || target.ExecutionEpoch != st.Epoch {
			return registry.ErrStaleObservation
		}
		x := nativeObservation(b, b.ExecutionRef(), registry.PowerHibernated, "")
		x.Native.HibernatedCapture = st.StatePoint
		x.RuntimeIdentity = nil
		return e.DB.ObserveMachine(ctx, b.ID, x)
	}
	if st.Power == "RUNNING" && st.Incarnation != "" && st.Epoch > b.ExecutionEpoch {
		current, err := control.DiscoverExecution(ctx, b.RuntimeName)
		if err != nil {
			return err
		}
		if current.InstanceID != st.Instance || current.ExecutionEpoch != st.Epoch || current.IncarnationID != st.Incarnation {
			return registry.ErrStaleObservation
		}
		inv, err := control.NativeInventory(ctx, current)
		if err != nil {
			return err
		}
		vm := inv.Execution
		if vm.ExecutionRef != current || vm.BundleDigest != b.RuntimeBundleID || vm.Phase != "registered" || vm.PIDStartTime <= 0 || vm.PID <= 0 || vm.HostBootID == "" || inv.Inventory.SourceDisposition != "RUNNING" {
			return registry.ErrStaleObservation
		}
		// A concurrent access can journal a newer authorization decision.
		// The registered VMM identifies the actual launch or restore receipt.
		if vm.LastOperationID != "" {
			operation = vm.LastOperationID
		}
		if operation == "" {
			return registry.ErrStaleObservation
		}
		op, err := control.QueryOperation(ctx, operation)
		if err != nil {
			return err
		}
		if op.Phase != "COMPLETED" || op.InstanceID != st.Instance || op.BundleDigest != b.RuntimeBundleID ||
			op.Execution == nil || *op.Execution != current ||
			(op.Kind != "launch" && (op.StatePoint == nil || op.StatePoint.ActivationDecision != "commit" || op.StatePoint.Stage != "released")) {
			return registry.ErrStaleObservation
		}
		profile := ""
		if inv.ManagedStandby() {
			profile = b.RuntimeBundleID + "/" + inv.Inventory.Protocol
		}
		x := nativeObservation(b, current, registry.PowerRunning, profile)
		x.Native.VerifiedLaunch = op.Kind == "launch"
		x.Native.VerifiedRestore = !x.Native.VerifiedLaunch
		x.Native.StopBarrier = &inv.Inventory.StopBarrier
		x.Native.ForkCapabilities = observedForkCapabilities(inv)
		x.RuntimeIdentity = JSON(rt.Identity{BootID: JSON(vm.HostBootID), PID: JSON(vm.PID), StartTime: JSON(vm.PIDStartTime)})
		return e.DB.ObserveMachine(ctx, b.ID, x)
	}
	return nil
}
