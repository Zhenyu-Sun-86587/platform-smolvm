package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"slices"
	"time"
)

type AccessEndpoint struct {
	Trigger     string
	Revision    int64
	ServiceID   string
	Reauthorize func(context.Context) error
	operationID string
}
type protectionReceipt struct {
	ID          string `json:"protection_id"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Revision    int64  `json:"revision"`
	Wake        string `json:"wake_operation"`
	Incarnation string `json:"incarnation_id"`
	Reason      string `json:"reason"`
}

const StatePointWakeFailed = "STATEPOINT_RESTORE_FAILED"

// CheckAccessPrincipal revalidates a live credential before continuing machine access.
func (e *Engine) CheckAccessPrincipal(ctx context.Context, p auth.Principal, machine string) error {
	return e.checkAccessPrincipal(ctx, p, machine, "")
}

func (e *Engine) checkAccessPrincipal(ctx context.Context, p auth.Principal, machine, serviceID string) error {
	var kind, scope string
	err := e.DB.ReadDB().QueryRowContext(ctx, `SELECT c.kind,c.scope_json FROM credentials c JOIN principals p ON p.id=c.principal_id
        WHERE c.id=? AND p.id=? AND p.disabled=0 AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>?)`,
		p.CredentialID, p.ID, time.Now().UnixMilli()).Scan(&kind, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if kind == "client_certificate" {
		var certificate struct {
			ServiceIDs []string `json:"service_ids"`
		}
		if serviceID == "" || p.Scope != (auth.Scope{Access: "tunnel", MachineID: machine}) ||
			json.Unmarshal([]byte(scope), &certificate) != nil || !slices.Contains(certificate.ServiceIDs, serviceID) {
			return auth.ErrForbidden
		}
		var enabled bool
		err := e.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(
            SELECT 1 FROM services WHERE id=? AND machine_id=? AND kind='tcp-mtls' AND enabled=1)`,
			serviceID, machine).Scan(&enabled)
		if err != nil {
			return err
		}
		if !enabled {
			return auth.ErrForbidden
		}
		return nil
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

// A control reply may be delayed while the Worker finishes a capture phase.
// Read calls and keyed mutations can use the same execution and request again
// before the access deadline. No Guest data has been submitted at this point.
type accessPolicyWorker struct{ policyWorker }

func (w accessPolicyWorker) Lifecycle(ctx context.Context, method, key string, expected workerclient.Execution, payload, into any) error {
	for {
		err := w.policyWorker.Lifecycle(ctx, method, key, expected, payload, into)
		if !workerclient.RetryDelivery(err) && !errors.Is(err, io.EOF) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), fmt.Errorf("waiting for Worker %s: %w", method, err))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *Engine) workerAccess(parent context.Context, p auth.Principal, b registry.Binding, client workerclient.LifecycleControl, entry AccessEndpoint) (registry.Binding, func(), error) {
	var none registry.Binding
	ctx, cancel := context.WithTimeout(parent, time.Duration(e.Config.Gateway.WaitSeconds)*time.Second)
	defer cancel()
	control := accessPolicyWorker{client}
	id, err := registry.NewID("access-")
	if err != nil {
		return none, nil, err
	}
	ack := protectionReceipt{}
	scope := WorkerExecution(b)
	enteredStopped := b.PowerState == registry.PowerStopped
	assigned := false
	var wakeHeld bool
	reauthorize := func() error {
		if err := e.checkAccessPrincipal(ctx, p, b.MachineID, entry.ServiceID); err != nil {
			return err
		}
		if entry.Reauthorize != nil {
			return entry.Reauthorize(ctx)
		}
		return nil
	}
	request := func(ctx context.Context, method, key string, args map[string]any, out *protectionReceipt) error {
		args["principal"] = p.ID
		args["protection_id"] = id
		return control.Lifecycle(ctx, method, key, scope, args, out)
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
		st, err := e.ObservePolicy(ctx, b, control)
		if err != nil {
			return none, nil, err
		}
		if b.PowerState == registry.PowerHibernated && (st.Power == "STOPPED" ||
			st.Power == "HIBERNATED" && st.StatePoint != b.LastCaptureID) {
			return none, nil, &Failure{StatePointWakeFailed, "Worker cannot confirm the binding's saved execution"}
		}
		scope = st.Execution()
		if ack.ID == "" {
			if err = reauthorize(); err != nil {
				return none, nil, err
			}
			args := map[string]any{"reason": entry.Trigger, "trigger": entry.Trigger,
				"expected_binding_revision": st.Revision, "authorization_revision": 1, "endpoint_revision": entry.Revision}
			if st.Power == "STOPPED" {
				if !enteredStopped {
					return none, nil, &Failure{Code: "STATE_CONFLICT", Message: "execution stopped while access was pending"}
				}
				policy, err := e.DB.LifecyclePolicy(ctx, m.ID)
				if err != nil {
					return none, nil, err
				}
				if !policy.Desired.Wake.ColdStart {
					return none, nil, &Failure{Code: "SERVICE_NOT_READY", Message: "cold wake is disabled"}
				}
				if st.Epoch == 0 {
					if !st.FirstLaunchSupported {
						return none, nil, &Failure{Code: "SERVICE_NOT_READY", Message: "this Worker requires an explicit first start"}
					}
					plan, err := e.FirstSystemLaunch(ctx, m, b)
					if err != nil {
						return none, nil, err
					}
					args["first_launch"] = plan
				}
			}
			if st.Power == "HIBERNATED" || st.Power == "STOPPED" {
				kind := rt.VMRestore
				if st.Power == "STOPPED" {
					kind = "launch"
				}
				wakeHeld, err = e.DB.BeginAccessWake(ctx, id, b, st.Epoch, kind)
				if err != nil {
					return none, nil, err
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
			err = request(ctx, "RequestWake", id+"-admit", args, &ack)
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
		if wakeHeld && ack.ID == id && ack.Wake != "" {
			if err := e.DB.ObserveAccessWakeDecision(ctx, id, ack.Wake); err != nil {
				return none, nil, err
			}
		}
		if ack.State == "released" {
			if ack.Reason == "statepoint_restore_failed" {
				return none, nil, &Failure{StatePointWakeFailed, "Worker could not restore the saved execution"}
			}
			return none, nil, auth.ErrForbidden
		}
		if ack.Kind == "wake" && st.Power != "RUNNING" {
			if err = reauthorize(); err != nil {
				return none, nil, err
			}
		}
		if ack.Wake != "" && ack.Kind == "wake" && st.Power == "RUNNING" {
			allowed := reauthorize()
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
			if b.ExecutionEpoch != scope.Epoch || b.IncarnationID != scope.IncarnationID || b.PowerState != registry.PowerRunning {
				adapter, err := e.ForBinding(b)
				if err != nil {
					return none, nil, err
				}
				if err = e.projectAutoPower(ctx, b, adapter, st, ack.Wake, entry.operationID); err != nil && !errors.Is(err, registry.ErrStaleObservation) {
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
			if err = reauthorize(); err != nil {
				return none, nil, err
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
func (e *Engine) projectAutoPower(ctx context.Context, b registry.Binding, adapter rt.Adapter, st WorkerLifecycleStatus, operation, owner string) error {
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
		return e.DB.ObserveOwnedMachine(ctx, b.ID, owner, x)
	}
	if st.Power == "RUNNING" && st.Incarnation != "" && (st.Epoch > b.ExecutionEpoch ||
		(st.Epoch == b.ExecutionEpoch && st.Incarnation == b.IncarnationID && b.PowerState == registry.PowerStandby)) {
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
		if current == b.ExecutionRef() {
			if !inv.ManagedStandby() {
				return registry.ErrStaleObservation
			}
			x := nativeObservation(b, current, registry.PowerRunning, b.RuntimeBundleID+"/"+inv.Inventory.Protocol)
			x.RuntimeIdentity = JSON(rt.Identity{BootID: JSON(vm.HostBootID), PID: JSON(vm.PID), StartTime: JSON(vm.PIDStartTime)})
			x.Native.ForkCapabilities = observedForkCapabilities(inv)
			return e.DB.ObserveOwnedMachine(ctx, b.ID, owner, x)
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
		return e.DB.ObserveOwnedMachine(ctx, b.ID, owner, x)
	}
	return nil
}
