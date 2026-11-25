package lifecycle

import (
	"context"
	"errors"
	"testing"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type nativePowerRuntime struct {
	createRuntime
	route          rt.BundleRoute
	execution      rt.ExecutionRef
	disposition    string
	submissions    int
	receipt        rt.OperationReceipt
	inventoryError error
}

func (n *nativePowerRuntime) DiscoverExecution(context.Context, string) (rt.ExecutionRef, error) {
	return n.execution, nil
}

func (n *nativePowerRuntime) NativeInventory(_ context.Context, ref rt.ExecutionRef) (rt.NativeInventory, error) {
	var result rt.NativeInventory
	if n.inventoryError != nil {
		return result, n.inventoryError
	}
	if ref != n.execution {
		return result, rt.ErrUnsupported
	}
	result.Execution.ExecutionRef = ref
	result.Execution.RouteID, result.Execution.BundleDigest, result.Execution.Phase = "deep", n.route.BundleID, "registered"
	result.Inventory.Protocol = rt.ManagedStandbyContract
	result.Inventory.SourceDisposition = n.disposition
	result.Inventory.MeasuredHelper = true
	result.Inventory.StandbyBlocker = []byte("null")
	result.Inventory.Devices = []rt.NativeDevice{{VirtioType: 2}}
	return result, nil
}

func (n *nativePowerRuntime) SubmitNative(_ context.Context, request rt.NativeSubmission) (rt.OperationReceipt, error) {
	if request.Expected != n.execution {
		return rt.OperationReceipt{}, rt.ErrUnsupported
	}
	n.submissions++
	n.disposition = "RUNNING"
	if request.Kind == rt.Standby {
		n.disposition = "STANDBY"
	}
	n.receipt = rt.OperationReceipt{
		OperationID: request.OperationID, Kind: request.Kind, RequestDigest: request.RequestDigest,
		RouteID: "deep", InstanceID: n.execution.InstanceID, BundleDigest: n.route.BundleID,
		ExpectedExecutionEpoch: n.execution.ExecutionEpoch, ExpectedIncarnationID: &n.execution.IncarnationID,
		Phase: "COMPLETED", Decision: "NONE", Revision: 5,
		Native: &rt.NativeProgress{Stage: "complete", Mode: "managed", SourceDisposition: n.disposition},
	}
	if request.Kind == rt.Standby {
		return rt.OperationReceipt{}, rt.ErrUnavailable
	}
	return n.receipt, nil
}

func (n *nativePowerRuntime) QueryOperation(_ context.Context, id string) (rt.OperationReceipt, error) {
	if id != n.receipt.OperationID {
		return rt.OperationReceipt{}, rt.ErrNotFound
	}
	return n.receipt, nil
}

func TestNativePowerReconcilesOriginalWorkerOperationAfterRestart(t *testing.T) {
	ctx := context.Background()
	official := &createRuntime{}
	engine, owner, epoch := runtimeTestEngine(t, ctx, official)
	db := engine.DB
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	execution := rt.ExecutionRef{InstanceID: "native-object", IncarnationID: "native-incarnation", ExecutionEpoch: 7}
	worker := &nativePowerRuntime{route: route, execution: execution, disposition: "RUNNING"}
	engine.runtimes = rt.NewResolver(official, map[rt.BundleRoute]rt.Adapter{route: worker})
	if err := db.RegisterRuntime(ctx, route, "isolation-socket", JSON(map[string]string{"schema": "RuntimeBundle/1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('m_native','native','t1',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,runtime_bundle_id)
		VALUES ('b_native','m_native',1,'native','current','ns_native','digest','serve','bundle');
		UPDATE machines SET current_binding_id='b_native' WHERE id='m_native';`); err != nil {
		t.Fatal(err)
	}
	request := registry.AcceptRequest{
		PrincipalID: owner, Epoch: epoch, Key: "try-standby", Scope: "POST /v1/machines/m_native/standby",
		Kind: rt.Standby, MachineID: "m_native", ExpectedRevision: 1, ExpectedGeneration: 1,
		AdmitRuntime: engine.AdmitRuntime, AdmitNative: engine.AdmitNativePower,
	}
	var capability *rt.CapabilityError
	if _, _, err := db.Accept(ctx, request); !errors.As(err, &capability) || capability.Status.Status != "unknown" {
		t.Fatalf("standby before native evidence = %v", err)
	}
	binding, err := db.GetBinding(ctx, "b_native")
	if err != nil || !engine.observeNativePower(ctx, binding, worker) {
		t.Fatal("failed to persist native binding", err)
	}
	standby, _, err := db.Accept(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	worker.inventoryError = rt.ErrUnavailable
	if err := engine.drive(ctx, standby); !errors.Is(err, rt.ErrUnavailable) {
		t.Fatalf("preflight while worker restarts = %v", err)
	}
	preflight, err := db.GetOperation(ctx, standby.ID)
	if err != nil || preflight.Stage != "preflight" || preflight.Effect != registry.EffectNone || worker.submissions != 0 {
		t.Fatal(preflight, worker.submissions, err)
	}
	worker.inventoryError = nil
	if err := engine.drive(ctx, preflight); err != nil {
		t.Fatal(err)
	}
	dispatched, err := db.GetOperation(ctx, standby.ID)
	if err != nil || dispatched.State != registry.Observing || dispatched.Effect != registry.EffectUnknown || worker.submissions != 1 {
		t.Fatal(dispatched, worker.submissions, err)
	}
	engine = New(db, engine.runtimes, engine.Config)
	if err := engine.drive(ctx, dispatched); err != nil {
		t.Fatal(err)
	}
	replayed, found, err := db.Accept(ctx, request)
	if err != nil || !found || replayed.ID != standby.ID || replayed.State != registry.Succeeded ||
		replayed.SourceDisposition != registry.SourceStandby || replayed.Effect != registry.EffectApplied || worker.submissions != 1 {
		t.Fatal(replayed, worker.submissions, found, err)
	}
	binding, err = db.GetBinding(ctx, "b_native")
	if err != nil || binding.PowerState != registry.PowerStandby || binding.ExecutionRef() != execution ||
		binding.GuestManagementStatus != "unknown" {
		t.Fatal(binding, err)
	}
	wakeRequest := request
	wakeRequest.Kind, wakeRequest.Key, wakeRequest.Scope = rt.Wake, "try-wake", "POST /v1/machines/m_native/wake"
	wakeRequest.ExpectedRevision = 2
	wake, _, err := db.Accept(ctx, wakeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.drive(ctx, wake); err != nil {
		t.Fatal(err)
	}
	result, err := db.GetOperation(ctx, wake.ID)
	if err != nil || result.State != registry.Succeeded || result.SourceDisposition != registry.SourceRunning || worker.submissions != 2 {
		t.Fatal(result, worker.submissions, err)
	}
	binding, err = db.GetBinding(ctx, "b_native")
	if err != nil || binding.PowerState != registry.PowerRunning || binding.ExecutionRef() != execution {
		t.Fatal(binding, err)
	}
	wrong := execution
	wrong.IncarnationID = "foreign-execution"
	if err := db.ObserveMachine(ctx, binding.ID, nativeObservation(binding, wrong, registry.PowerStandby, binding.CapabilityProfileID)); !errors.Is(err, registry.ErrStaleObservation) {
		t.Fatalf("foreign observation admitted: %v", err)
	}
}
