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
	cancels     int
	retryAborts bool
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
	for i := range n.forkReceipt.Fork.Children {
		failed := &n.forkReceipt.Fork.Children[i]
		if failed.Phase != "ABORTED" || failed.Cancelled || failed.AdmissionError != "" {
			continue
		}
		failed.Attempt++
		failed.ExecutionEpoch++
		failed.OperationID += "-retry"
		inc := fmt.Sprintf("child-vmm-retry-%d", i)
		failed.IncarnationID = &inc
		failed.Phase, failed.ActivationDecision = "COMPLETED", "commit"
		if n.retryAborts {
			failed.Phase, failed.ActivationDecision = "ABORTED", "none"
		}
		n.references[failed.Name] = rt.ExecutionRef{InstanceID: failed.InstanceID, IncarnationID: inc, ExecutionEpoch: failed.ExecutionEpoch}
	}
	return rt.OperationReceipt{}, rt.ErrUnavailable
}

func (n *forkRuntime) CancelFork(_ context.Context, id string, control rt.ForkControl) (rt.OperationReceipt, error) {
	if id != n.forkReceipt.OperationID || control.RequestDigest != n.forkReceipt.RequestDigest ||
		control.Expected != n.references["source"] || control.ExpectedRevision != n.forkReceipt.Revision {
		return rt.OperationReceipt{}, rt.ErrInvalid
	}
	n.cancels++
	n.forkReceipt.Revision++
	for i := range n.forkReceipt.Fork.Children {
		child := &n.forkReceipt.Fork.Children[i]
		if child.Phase == "NEW" {
			child.Cancelled, child.Phase = true, "ABORTED"
		}
	}
	n.forkReceipt.Phase, n.forkReceipt.Fork.Stage = "COMPLETED", "complete"
	return rt.OperationReceipt{}, rt.ErrUncertain
}

func nativeForkFixture(t *testing.T, ctx context.Context) (*Engine, *forkRuntime, registry.AcceptRequest) {
	t.Helper()
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
	return engine, worker, request
}

func TestNativeForkKeepsPartialChildrenAfterSourceRemoval(t *testing.T) {
	ctx := context.Background()
	engine, worker, request := nativeForkFixture(t, ctx)
	db, owner, epoch := engine.DB, request.PrincipalID, request.Epoch
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
	worker.forkReceipt.Fork.Children[1].ActivationDecision = "commit"
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

func TestNativeForkFailedEffectsPreserveAbortedActivationAcrossRetry(t *testing.T) {
	for _, tc := range []struct {
		name, decision, effect string
	}{
		{"legacy_receipt", "", registry.EffectNone},
		{"aborted_before_commit", "none", registry.EffectNone},
		{"one_committed_attempt", "commit", registry.EffectUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			engine, worker, request := nativeForkFixture(t, ctx)
			db := engine.DB
			accepted, _, err := db.Accept(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			read := func() registry.Operation {
				t.Helper()
				op, err := db.GetOperation(ctx, accepted.ID)
				if err != nil {
					t.Fatal(err)
				}
				return op
			}
			drive := func() {
				t.Helper()
				if err := engine.drive(ctx, read()); err != nil {
					t.Fatal(err)
				}
			}
			drive()
			worker.forkReceipt.Phase, worker.forkReceipt.Fork.Stage = "COMPLETED", "complete"
			worker.forkReceipt.Fork.Children[0].Phase = "ABORTED"
			worker.forkReceipt.Fork.Children[1].Phase = "ABORTED"
			worker.forkReceipt.Fork.Children[1].ActivationDecision = tc.decision
			drive()
			drive()
			failed, replay, err := db.Accept(ctx, request)
			if err != nil || !replay || failed.State != registry.Failed || failed.Effect != tc.effect || !failed.SourceLockReleased ||
				failed.Children[0].State != "quarantined" || failed.Children[1].State != "quarantined" || worker.submissions != 1 {
				t.Fatal(failed, replay, worker.submissions, err)
			}
			if tc.effect != registry.EffectUnknown {
				return
			}
			worker.retryAborts = true
			envelope := registry.MutationRequest{PrincipalID: request.PrincipalID, Epoch: request.Epoch, Key: "fenced-retry",
				Scope: "POST /v1/operations/" + failed.ID + "/retry", Request: JSON(struct{}{}), ExpectedRevision: failed.Version}
			if _, _, err := db.Mutate(ctx, envelope, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
				op, err := engine.RetryNativeFork(ctx, tx, failed, failed.Version)
				return registry.MutationResult{Response: JSON(op)}, err
			}); err != nil {
				t.Fatal(err)
			}
			if err := engine.drive(ctx, read()); err != rt.ErrUnavailable || worker.retries != 1 {
				t.Fatal(err, worker.retries)
			}
			engine = New(db, engine.runtimes, engine.Config)
			drive()
			drive()
			retried := read()
			if retried.ID != failed.ID || retried.State != registry.Failed || retried.Effect != registry.EffectUnknown ||
				retried.Children[0].Attempt != 1 || retried.Children[1].Attempt != 1 || worker.submissions != 1 || worker.retries != 1 {
				t.Fatal(retried, worker.submissions, worker.retries)
			}
		})
	}
}

func TestNativeForkValidatesOptionalActivationDecision(t *testing.T) {
	submission := rt.ForkSubmission{OperationID: "fork", Expected: rt.ExecutionRef{InstanceID: "parent", IncarnationID: "parent-execution"}}
	submission.Parameters.Children = []string{"child"}
	incarnation := "child-execution"
	for _, tc := range []struct {
		decision    string
		incarnation *string
		valid       bool
	}{
		{"", nil, true},
		{"none", &incarnation, true},
		{"commit", &incarnation, true},
		{"commit", nil, false},
		{"commit", &submission.Expected.IncarnationID, false},
		{"unrecognized", &incarnation, false},
	} {
		child := rt.ForkChildReceipt{Name: "child", InstanceID: "child-slot", OperationID: "attempt-1", Phase: "ABORTED",
			ExecutionEpoch: 1, IncarnationID: tc.incarnation, ActivationDecision: tc.decision}
		if err := verifyForkReceiptChildren(submission, []rt.ForkChildReceipt{child}); (err == nil) != tc.valid {
			t.Fatal(child, err)
		}
	}
}

func TestNativeForkRetainsKnownActivationAfterReceiptRegression(t *testing.T) {
	ctx := context.Background()
	engine, worker, request := nativeForkFixture(t, ctx)
	accepted, _, err := engine.DB.Accept(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	read := func() registry.Operation {
		t.Helper()
		op, err := engine.DB.GetOperation(ctx, accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	if err := engine.drive(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	worker.forkReceipt.Fork.Children[0].Phase = "NEW"
	worker.forkReceipt.Fork.Children[0].ActivationDecision = "commit"
	if err := engine.drive(ctx, read()); err != nil {
		t.Fatal(err)
	}
	worker.forkReceipt.Revision++
	worker.forkReceipt.Phase, worker.forkReceipt.Fork.Stage = "COMPLETED", "complete"
	worker.forkReceipt.Fork.Children[0].Phase = "ABORTED"
	worker.forkReceipt.Fork.Children[0].ActivationDecision = "none"
	worker.forkReceipt.Fork.Children[1].Phase = "ABORTED"
	if err := engine.drive(ctx, read()); err != nil {
		t.Fatal(err)
	}
	if result := read(); result.State != registry.RecoveryRequired || result.Effect != registry.EffectUnknown {
		t.Fatal(result)
	}
}

func TestNativeForkCancellationOwnsOnlyUncommittedTargets(t *testing.T) {
	for _, stage := range []string{"preflight", "children", "partial"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			engine, worker, req := nativeForkFixture(t, ctx)
			db := engine.DB
			op, _, err := db.Accept(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			read := func() registry.Operation {
				t.Helper()
				got, err := db.GetOperation(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			drive := func() {
				t.Helper()
				if err := engine.drive(ctx, read()); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "preflight" {
				_, err = db.Transition(ctx, op.ID, registry.TransitionRequest{State: registry.Dispatching,
					Stage: "preflight", Dispatch: JSON(Intent{Stage: "preflight"})})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				drive()
				if stage == "children" {
					worker.forkReceipt.Fork.Children[0].Phase = "NEW"
				}
				drive()
				drive()
			}
			current := read()
			envelope := registry.MutationRequest{PrincipalID: req.PrincipalID, Epoch: req.Epoch, Key: "cancel",
				Scope: "POST /v1/operations/" + op.ID + "/cancel", Request: JSON(struct{}{}), ExpectedRevision: current.Version}
			apply := func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
				out, err := engine.CancelNativeForkTx(ctx, tx, op.ID, current.Version)
				return registry.MutationResult{Response: JSON(out)}, err
			}
			first, replay, err := db.Mutate(ctx, envelope, apply)
			if err != nil || replay {
				t.Fatal(err, replay)
			}
			if stage != "preflight" {
				if read().Stage != "fork_cancel_requested" || engine.drive(ctx, read()) != rt.ErrUncertain {
					t.Fatal(read())
				}
				engine = New(db, engine.runtimes, engine.Config)
				drive()
			}
			final := read()
			if stage == "partial" {
				if final.State != registry.Succeeded || final.Effect != registry.EffectPartial || final.Children[0].State != "ready" ||
					final.Children[0].MachineID != op.Children[0].MachineID || final.Children[1].ErrorCode != "FORK_CHILD_CANCELLED" {
					t.Fatal(final)
				}
			} else if final.State != registry.Cancelled || final.Effect != registry.EffectNone {
				t.Fatal(final)
			}
			wantCalls := 1
			if stage == "preflight" {
				wantCalls = 0
			}
			if worker.submissions != wantCalls || worker.cancels != wantCalls {
				t.Fatal(worker.submissions, worker.cancels)
			}
			last, replay, err := db.Mutate(ctx, envelope, apply)
			if err != nil || !replay || string(last) != string(first) {
				t.Fatal(err, replay, string(last))
			}
		})
	}
}
