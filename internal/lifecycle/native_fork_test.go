package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type forkRuntime struct {
	nativePowerRuntime
	references  map[string]rt.ExecutionRef
	forkReceipt rt.OperationReceipt
	retries     int
}

func (n *forkRuntime) DiscoverExecution(_ context.Context, name string) (rt.ExecutionRef, error) {
	return n.references[name], nil
}

func (n *forkRuntime) NativeInventory(_ context.Context, ref rt.ExecutionRef) (rt.NativeInventory, error) {
	var result rt.NativeInventory
	result.Execution.ExecutionRef = ref
	result.Execution.RouteID, result.Execution.BundleDigest, result.Execution.Phase = "deep", n.route.BundleID, "registered"
	result.Execution.PID, result.Execution.PIDStartTime, result.Execution.HostBootID = 5, 12, "boot"
	result.Capabilities = map[string]rt.Capability{
		"state.independent_fork": {Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: rt.NativeForkProfile},
		"state.fork_batch":       {Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: rt.NativeForkProfile},
	}
	result.Inventory.Protocol, result.Inventory.SourceDisposition, result.Inventory.MeasuredHelper = rt.ManagedStandbyContract, "RUNNING", true
	result.Inventory.StandbyBlocker = json.RawMessage("null")
	var device rt.NativeDevice
	err := json.Unmarshal([]byte(fmt.Sprintf(`{"virtio_type":2,"block":{"read_only":false,"managed_attachment":{
		"attachment_id":%q,"device_id":"root","role":"root","volume_id":%q,"generation_id":"base",
		"writable_head_id":%q,"grant_id":%q,"execution_epoch":%d}}}`,
		ref.InstanceID+"-disk", ref.InstanceID+"-volume", ref.InstanceID+"-head", ref.InstanceID+"-grant", ref.ExecutionEpoch)), &device)
	result.Inventory.Devices = []rt.NativeDevice{device}
	return result, err
}

func (n *forkRuntime) SubmitFork(_ context.Context, input rt.ForkSubmission) (rt.OperationReceipt, error) {
	n.submissions++
	progress := &rt.ForkProgress{Stage: "children", CaptureID: "one-source-cut", SourceReleased: true, OriginalSourceDisposition: "RUNNING"}
	for i, name := range input.Parameters.Children {
		ref := rt.ExecutionRef{InstanceID: fmt.Sprintf("child-%d", i), IncarnationID: fmt.Sprintf("child-vmm-%d", i), ExecutionEpoch: 1}
		n.references[name] = ref
		progress.Children = append(progress.Children, rt.ForkChildReceipt{Name: name, InstanceID: ref.InstanceID,
			OperationID: fmt.Sprintf("worker-child-%d", i), Phase: "NEW", IncarnationID: &ref.IncarnationID, ExecutionEpoch: 1})
	}
	progress.Children[0].Phase = "COMPLETED"
	n.forkReceipt = rt.OperationReceipt{OperationID: input.OperationID, RequestDigest: input.RequestDigest, Kind: input.Kind,
		RouteID: "deep", InstanceID: input.Expected.InstanceID, ExpectedExecutionEpoch: input.Expected.ExecutionEpoch,
		ExpectedIncarnationID: &input.Expected.IncarnationID, BundleDigest: n.route.BundleID, Phase: "NEW", Decision: "NONE", Revision: 1, Fork: progress}
	return rt.OperationReceipt{}, rt.ErrUnavailable
}

func (n *forkRuntime) QueryOperation(_ context.Context, id string) (rt.OperationReceipt, error) {
	if id != n.forkReceipt.OperationID {
		return rt.OperationReceipt{}, &rt.Error{Code: "NOT_FOUND", NativeCode: "E_NOT_FOUND"}
	}
	return n.forkReceipt, nil
}

func (n *forkRuntime) RetryFork(_ context.Context, id string, control rt.ForkControl) (rt.OperationReceipt, error) {
	n.retries++
	if id != n.forkReceipt.OperationID || control.RequestDigest != n.forkReceipt.RequestDigest ||
		control.Expected.InstanceID != n.forkReceipt.InstanceID || control.Expected.ExecutionEpoch != n.forkReceipt.ExpectedExecutionEpoch ||
		control.ExpectedRevision != n.forkReceipt.Revision {
		return rt.OperationReceipt{}, rt.ErrInvalid
	}
	n.forkReceipt.Revision++
	failed := &n.forkReceipt.Fork.Children[1]
	failed.Attempt++
	failed.ExecutionEpoch++
	failed.OperationID += "-retry"
	inc := "child-vmm-retry"
	failed.IncarnationID = &inc
	failed.Phase = "COMPLETED"
	n.references[failed.Name] = rt.ExecutionRef{InstanceID: failed.InstanceID, IncarnationID: inc, ExecutionEpoch: failed.ExecutionEpoch}
	return rt.OperationReceipt{}, rt.ErrUnavailable
}

func TestNativeForkKeepsPartialChildrenAfterSourceRemoval(t *testing.T) {
	ctx := context.Background()
	official := &createRuntime{}
	engine, owner, epoch := runtimeTestEngine(t, ctx, official)
	engine.Config.StateDir = t.TempDir()
	engine.Config.Storage.MinBytes, engine.Config.Storage.MinPercent = 0, 0
	db := engine.DB
	source := rt.ExecutionRef{InstanceID: "source", IncarnationID: "source-vmm", ExecutionEpoch: 8}
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "managed"}
	worker := &forkRuntime{references: map[string]rt.ExecutionRef{"source": source}}
	worker.route = route
	engine.runtimes = rt.NewResolver(official, map[rt.BundleRoute]rt.Adapter{route: worker})
	if err := db.RegisterRuntime(ctx, route, "fork-socket", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('source','source','t1',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,runtime_bundle_id)
		VALUES ('source-bind','source',1,'source','current','source-ns','digest','serve','managed');
		UPDATE machines SET current_binding_id='source-bind' WHERE id='source';`); err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetBinding(ctx, "source-bind")
	if err != nil || !engine.observeNativePower(ctx, binding, worker) {
		t.Fatal("source lacks verified fork evidence", err)
	}
	request := registry.AcceptRequest{Kind: rt.ForkBatch, PrincipalID: owner, Epoch: epoch, Key: "fork-key", Scope: "POST /v1/machines/source/fork-batches",
		MachineID: "source", ExpectedGeneration: 1, Heavy: true, Request: json.RawMessage(`{"children":[{"name":"first"},{"name":"second"}]}`),
		ForkChildren: []string{"first", "second"}, AdmitRuntime: engine.AdmitRuntime, AdmitNative: engine.AdmitNativeFork}
	accepted, _, err := db.Accept(ctx, request)
	if err != nil || len(accepted.Children) != 2 {
		t.Fatal(accepted, err)
	}
	read := func() registry.Operation {
		t.Helper()
		op, err := db.GetOperation(ctx, accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	drive := func(op registry.Operation) {
		t.Helper()
		if err := engine.drive(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	drive(accepted)
	if op := read(); op.State != registry.Observing || op.Effect != registry.EffectUnknown || op.SourceLockReleased {
		t.Fatal(op)
	}
	engine = New(db, engine.runtimes, engine.Config)
	drive(read())
	if op := read(); !op.SourceLockReleased || op.SourceDisposition != registry.SourceRunning || op.Children[0].State != "pending" {
		t.Fatal(op)
	}
	machine, err := db.GetMachine(ctx, "source")
	if err != nil || machine.ActiveOperationID != "" {
		t.Fatal(machine, err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE machines SET current_binding_id=NULL,deleted_at=1 WHERE id='source';
		UPDATE runtime_bindings SET status='retired' WHERE id='source-bind'`); err != nil {
		t.Fatal(err)
	}
	drive(read())
	partial := read()
	if partial.State != registry.Observing || partial.Effect != registry.EffectPartial || partial.Children[0].ResourceIndependence != "verified" {
		t.Fatal(partial)
	}
	machine, err = db.GetMachine(ctx, partial.Children[0].MachineID)
	if err != nil || machine.ActiveOperationID != "" || machine.CurrentBinding == nil || machine.CurrentBinding.PowerState != registry.PowerRunning {
		t.Fatal(machine, err)
	}
	worker.forkReceipt.Phase, worker.forkReceipt.Fork.Stage, worker.forkReceipt.Fork.Children[1].Phase = "COMPLETED", "complete", "ABORTED"
	worker.forkReceipt.Revision++
	drive(partial)
	replayed, found, err := db.Accept(ctx, request)
	if err != nil || !found || replayed.ID != accepted.ID || worker.submissions != 1 || replayed.Effect != registry.EffectPartial ||
		replayed.State != registry.Succeeded || replayed.Children[0].MachineID != accepted.Children[0].MachineID ||
		replayed.Children[1].MachineID != accepted.Children[1].MachineID || replayed.Children[1].State != "quarantined" {
		t.Fatal(replayed, found, worker.submissions, err)
	}
	previous := read()
	envelope := registry.MutationRequest{PrincipalID: owner, Epoch: epoch, Key: "retry-key", Scope: "POST /v1/operations/" + previous.ID + "/retry", Request: JSON(struct{}{}), ExpectedRevision: previous.Version}
	apply := func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		op, err := engine.RetryNativeFork(ctx, tx, previous, previous.Version)
		return registry.MutationResult{Response: JSON(op)}, err
	}
	if _, replay, err := db.Mutate(ctx, envelope, apply); err != nil || replay {
		t.Fatal(replay, err)
	}
	if err := engine.drive(ctx, read()); err != rt.ErrUnavailable || worker.retries != 1 {
		t.Fatal(worker.retries, err)
	}
	engine = New(db, engine.runtimes, engine.Config)
	drive(read())
	if op := read(); op.Children[0].State != "ready" || op.Children[1].State != "pending" || op.Children[1].Attempt != 1 {
		t.Fatal(op)
	}
	drive(read())
	completed := read()
	if completed.State != registry.Succeeded || completed.Effect != registry.EffectApplied || worker.retries != 1 || !completed.SourceLockReleased ||
		completed.Children[0].MachineID != previous.Children[0].MachineID || completed.Children[0].Attempt != 0 ||
		completed.Children[1].MachineID != previous.Children[1].MachineID || completed.Children[1].State != "ready" {
		t.Fatal(completed, worker.retries)
	}
	if _, replay, err := db.Mutate(ctx, envelope, apply); err != nil || !replay {
		t.Fatal(replay, err)
	}
}
