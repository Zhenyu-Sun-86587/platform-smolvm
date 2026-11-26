package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type placementFixture struct {
	db                 *registry.Store
	path, actor, epoch string
}

func placementDB(t *testing.T) placementFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "platform.db")
	db, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := auth.New(db).Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	control, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "template", Name: "System", Version: 1, ImageDigest: "example@sha256:test"}); err != nil {
		t.Fatal(err)
	}
	return placementFixture{db, path, owner.ID, control.ControlEpoch}
}

func (f placementFixture) worker(t *testing.T, name string) placement.Sample {
	t.Helper()
	ctx := context.Background()
	if err := f.db.RegisterWorker(ctx, registry.Worker{ID: name, Name: name}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: name, Generation: 1, Authority: f.epoch, CertificateFingerprint: "certificate-" + name}); err != nil {
		t.Fatal(err)
	}
	route := rt.BundleRoute{WorkerID: name, InstanceID: "global-" + name, LocalInstanceID: "serve", BundleID: "bundle"}
	if err := f.db.RegisterRuntime(ctx, route, "unix:/run/serve.sock", json.RawMessage(`{"contracts":{"state_reader_id":"reader"}}`)); err != nil {
		t.Fatal(err)
	}
	return placement.Sample{
		WorkerID: name, ControlEpoch: f.epoch, EnrollmentGeneration: 1, Reconciled: true,
		Session:  placement.Session{HostBootID: "boot-" + name, BrokerSessionID: "broker-" + name, NodeSessionID: "node-" + name},
		Sequence: 1, QueryNonce: "query-1", Source: placement.SourceWorkerBudget,
		Budget:   placement.Budget{Capacity: placement.Resources{VCPUs: 8, MemoryMiB: 8 * 1024, StorageBytes: 1 << 38, HeavySlots: 2}},
		Pressure: &placement.Pressure{AvailableMemoryMiB: 8 * 1024, DiskFreeBytes: 1 << 38},
		Profiles: []placement.RuntimeProfile{{RuntimeInstanceID: route.InstanceID, LocalInstanceID: route.LocalInstanceID, BundleID: route.BundleID,
			Reader: "reader", Architecture: "x86_64", Hypervisor: "kvm", RuntimeProcessID: "serve-process", Ready: true,
			Capabilities: []string{rt.SystemBootCapability}, Artifacts: []string{"image"}}},
	}
}

func savePlacementSample(t *testing.T, db *registry.Store, sample placement.Sample) {
	t.Helper()
	if err := db.RecordWorkerSample(context.Background(), sample, queryFor(sample)); err != nil {
		t.Fatal(err)
	}
}

func queryFor(sample placement.Sample) placement.Query {
	started := time.Now()
	return placement.Query{Nonce: sample.QueryNonce, StartedAt: started, CompletedAt: time.Now()}
}

func (f placementFixture) request(key string) registry.AcceptRequest {
	return registry.AcceptRequest{PrincipalID: f.actor, PrincipalScope: []byte(`{"access":"manage"}`), Epoch: f.epoch,
		Key: key, Scope: "POST /v1/machines", Kind: "create", Name: key, TemplateID: "template", Request: []byte(`{"start":true}`),
		Placement: &placement.Request{Mode: placement.Auto, Architecture: "x86_64", Hypervisor: "kvm", Reader: "reader",
			Capabilities: []string{rt.SystemBootCapability}, ArtifactDigest: "image",
			Resources: placement.Resources{VCPUs: 2, MemoryMiB: 6 * 1024, HeavySlots: 1},
			Headroom:  placement.Resources{MemoryMiB: 256}},
		AdmitRuntime: func(rt.BundleRoute, string) error { return nil },
	}
}

func TestPlacementSerializesAcceptAndHolds(t *testing.T) {
	f := placementDB(t)
	savePlacementSample(t, f.db, f.worker(t, "a"))
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"first", "second"} {
		wg.Go(func() {
			_, _, err := f.db.Accept(ctx, f.request(key))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var accepted, rejected int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, placement.ErrNoCandidate):
			rejected++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	savePlacementSample(t, f.db, f.worker(t, "b"))
	op, _, err := f.db.Accept(ctx, f.request("third"))
	if err != nil || op.WorkerID != "b" || op.RuntimeInstanceID != "global-b" {
		t.Fatalf("second Worker selection = %+v, %v", op, err)
	}
	machine, err := f.db.GetMachine(ctx, op.MachineID)
	if err != nil || machine.WorkerID != "b" || !json.Valid(machine.Spec) {
		t.Fatalf("persisted placement = %+v, %v", machine, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(machine.Spec, &spec); err != nil || spec["runtime_instance_id"] != "global-b" {
		t.Fatalf("resolved create spec = %v, %v", spec, err)
	}
}

func TestPlacementUnknownRechecksOriginalAndRequiresCausalBudget(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	savePlacementSample(t, f.db, a)
	savePlacementSample(t, f.db, f.worker(t, "b"))
	req := f.request("first")
	op, _, err := f.db.Accept(ctx, req)
	if err != nil || op.WorkerID != "a" {
		t.Fatalf("stable first placement = %+v, %v", op, err)
	}
	dispatch := registry.TransitionRequest{State: registry.Dispatching, Stage: "worker_launch", Dispatch: []byte(`{"launch":{"request_digest":"sha256:native"}}`)}
	if _, err := f.db.Transition(ctx, op.ID, dispatch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Transition(ctx, op.ID, registry.TransitionRequest{State: registry.RecoveryRequired, Stage: "verify", Effect: registry.EffectUnknown}); err != nil {
		t.Fatal(err)
	}
	assertPhase := func(want string) {
		t.Helper()
		r, err := f.db.GetPlacementReservation(ctx, op.ID)
		if err != nil || r.WorkerID != "a" || r.Phase != want {
			t.Fatalf("reservation = %+v, want %s, err=%v", r, want, err)
		}
	}
	assertPhase(placement.Unknown)
	replay, replayed, err := f.db.Accept(ctx, req)
	if err != nil || !replayed || replay.ID != op.ID || replay.WorkerID != "a" {
		t.Fatalf("replay = %+v (%t), %v", replay, replayed, err)
	}
	req = f.request("pinned")
	req.Placement.Mode, req.Placement.WorkerID = placement.Pinned, "a"
	if _, _, err := f.db.Accept(ctx, req); !errors.Is(err, placement.ErrNoCandidate) {
		t.Fatalf("unknown reservation did not hold capacity: %v", err)
	}
	proof := placement.Admission{
		OperationID: op.ID, Attempt: 1, WorkerID: "a", RuntimeInstanceID: "global-a", BundleID: "bundle", ControlEpoch: f.epoch,
		EnrollmentGeneration: 1, Session: a.Session, NativeDigest: "sha256:native", ReceiptID: op.ID,
		Kind: "wake_demand_ack", ConfirmedThroughSequence: 2,
	}
	if err := f.db.RecordPlacementAdmission(ctx, proof); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("wake demand ack accepted: %v", err)
	}
	proof.Kind = placement.AdmissionOperation
	proof.WorkerID = "b"
	if err := f.db.RecordPlacementAdmission(ctx, proof); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("accepted proof from different owner: %v", err)
	}
	proof.WorkerID = "a"
	if err := f.db.RecordPlacementAdmission(ctx, proof); err != nil {
		t.Fatal(err)
	}
	a.Sequence, a.QueryNonce = 2, "query-2"
	savePlacementSample(t, f.db, a)
	assertPhase(placement.Admitted)
	a.Sequence, a.QueryNonce = 3, "query-3"
	a.Budget.Allocated.MemoryMiB = 6 * 1024
	savePlacementSample(t, f.db, a)
	assertPhase(placement.Covered)
	req.Placement.Resources.MemoryMiB = 1024
	if _, _, err := f.db.Accept(ctx, req); err != nil {
		t.Fatalf("covered reservation was charged twice: %v", err)
	}
	old := a
	a.Session.BrokerSessionID, a.Sequence, a.QueryNonce = "restarted-broker", 1, "broker-restarted"
	savePlacementSample(t, f.db, a)
	assertPhase(placement.Unknown)
	old.Sequence, old.QueryNonce = 4, "query-retired"
	if err := f.db.RecordWorkerSample(ctx, old, queryFor(old)); !errors.Is(err, registry.ErrStaleObservation) {
		t.Fatalf("accepted replay of retired broker sample: %v", err)
	}
	if err := f.db.RecordPlacementAdmission(ctx, proof); !errors.Is(err, registry.ErrStaleObservation) {
		t.Fatalf("accepted admission from retired broker session: %v", err)
	}
}

func TestKnownOnlyDrainBlocksAndRecoveryRequiresRebind(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	savePlacementSample(t, f.db, a)
	worker, err := f.db.GetWorker(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	cordon, err := f.db.SetWorkerMaintenance(ctx, "a", worker.Revision, "cordoned", "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetWorkerMaintenance(ctx, "a", worker.Revision, "draining", "restart"); !errors.Is(err, registry.ErrRevision) {
		t.Fatalf("stale maintenance CAS: %v", err)
	}
	if err := f.db.CheckWorkerEntrance(ctx, "a", "start"); err != nil {
		t.Fatal(err)
	}
	drain, err := f.db.SetWorkerMaintenance(ctx, "a", cordon.Revision, "draining", "restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CheckWorkerEntrance(ctx, "a", "access"); !errors.Is(err, registry.ErrWorkerMaintenance) {
		t.Fatalf("Drain does not gate access: %v", err)
	}
	proof := registry.WorkerMaintenanceEvidence{Coverage: registry.InventoryKnownOnly, GateMode: "draining", GateRevision: 2,
		ControlEpoch: f.epoch, Session: a.Session, BudgetSequence: a.Sequence, CollectorsActive: true, ExternalEntrancesClosed: true}
	drain, err = f.db.RecordWorkerMaintenanceEvidence(ctx, "a", drain.Revision, proof)
	if err != nil || drain.Phase != "draining_blocked" {
		t.Fatalf("known-only drain = %+v, %v", drain, err)
	}
	if _, err := f.db.SetWorkerMaintenance(ctx, "a", drain.Revision, "drained", "restart"); !errors.Is(err, registry.ErrWorkerMaintenance) {
		t.Fatalf("known-only permitted drained: %v", err)
	}
	if _, err := f.db.SetWorkerMaintenance(ctx, "a", drain.Revision, "decommissioned", "remove"); !errors.Is(err, registry.ErrWorkerMaintenance) {
		t.Fatalf("known-only permitted decommission: %v", err)
	}
	ctl, err := f.db.BeginRecovery(ctx, "restored database")
	if err != nil || ctl.ControlEpoch == f.epoch {
		t.Fatalf("recovery epoch = %+v, %v", ctl, err)
	}
	if _, err := f.db.SetMaintenance(ctx, false, ""); !errors.Is(err, registry.ErrMaintenance) {
		t.Fatalf("recovery completed without rebind: %v", err)
	}
	oldEnrollment := registry.WorkerEnrollment{WorkerID: "a", Generation: 1, Authority: f.epoch, CertificateFingerprint: "certificate-a"}
	if err := f.db.EnrollWorker(ctx, oldEnrollment); !errors.Is(err, registry.ErrControlEpoch) {
		t.Fatalf("old controller enrolled: %v", err)
	}
	newEnrollment := registry.WorkerEnrollment{WorkerID: "a", Generation: 2, Authority: ctl.ControlEpoch, CertificateFingerprint: "certificate-next"}
	if err := f.db.EnrollWorker(ctx, newEnrollment); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetMaintenance(ctx, false, ""); !errors.Is(err, registry.ErrMaintenance) {
		t.Fatalf("enrollment alone lifted recovery: %v", err)
	}
	a.EnrollmentGeneration, a.ControlEpoch, a.QueryNonce = 2, ctl.ControlEpoch, "fresh-authority"
	a.Session.NodeSessionID = "replacement-node-session"
	savePlacementSample(t, f.db, a)
	if err := f.db.ConfirmWorkerAuthority(ctx, registry.WorkerAuthorityProof{WorkerID: "a", ControlEpoch: ctl.ControlEpoch, EnrollmentGeneration: 2,
		Session: a.Session, BudgetSequence: a.Sequence, Coverage: registry.InventoryKnownOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetMaintenance(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
}

func (f placementFixture) hibernatedBinding(t *testing.T, worker string) registry.Binding {
	t.Helper()
	ctx := context.Background()
	req := f.request("source")
	req.Placement = nil
	req.RuntimeInstanceID = "global-" + worker
	req.Request = []byte(`{"resources":{"cpu_count":2,"memory_mib":6144}}`)
	op, _, err := f.db.Accept(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE runtime_bindings SET status='current',
		runtime_instance_object_id='source', execution_epoch=1, incarnation_id='saved', power_state='hibernated',last_capture_id='capture'
		WHERE id=?`, op.BindingID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE machines SET current_binding_id=?,binding_generation=1 WHERE id=?`, op.BindingID, op.MachineID); err != nil {
		t.Fatal(err)
	}
	b, err := f.db.GetBinding(ctx, op.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngressWakeDemandOnlyFoldsAfterPinnedRestoreAndNewBudget(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	a.Profiles[0].Capabilities = append(a.Profiles[0].Capabilities, rt.FullStateOperationCapability(rt.VMRestore))
	savePlacementSample(t, f.db, a)
	b := f.hibernatedBinding(t, "a")
	for _, id := range []string{"demand-a", "demand-b"} {
		if held, err := f.db.BeginAccessWake(ctx, id, b, 1, rt.VMRestore); !held || err != nil {
			t.Fatalf("wake ingress held=%t err=%v", held, err)
		}
		if err := f.db.ObserveAccessWakeDecision(ctx, id, "actual-restore"); err != nil {
			t.Fatal(err)
		}
	}
	checks, err := f.db.ListAccessWakeChecks(ctx, "a")
	if err != nil || len(checks) != 2 || checks[0].HoldID != checks[1].HoldID {
		t.Fatalf("same execution wake holds: %+v, %v", checks, err)
	}
	check := checks[0]
	req := f.request("another-large-request")
	if _, _, err := f.db.Accept(ctx, req); !errors.Is(err, placement.ErrNoCandidate) {
		t.Fatalf("WakeDemand acknowledgement dropped its hold: %v", err)
	}
	have := rt.OperationReceipt{OperationID: "actual-restore", Kind: rt.VMRestore,
		RequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RouteID: "deep",
		InstanceID: "source", BundleDigest: "bundle", ExpectedExecutionEpoch: 1, Phase: "COMPLETED", Revision: 12,
		Execution:  &rt.ExecutionRef{InstanceID: "source", ExecutionEpoch: 2, IncarnationID: "fresh"},
		StatePoint: &rt.StatePointProgress{CaptureID: "capture", ActivationDecision: "commit", Stage: "released"}}
	if err := f.db.RecordAccessWakeAdmission(ctx, check, have, a, a.Sequence); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("same sequence folded Restore hold: %v", err)
	}
	a.Sequence, a.QueryNonce = 2, "budget_after_admission"
	a.Budget.Allocated.MemoryMiB = 6 * 1024
	savePlacementSample(t, f.db, a)
	wrong := have
	wrong.Kind = "launch"
	if err := f.db.RecordAccessWakeAdmission(ctx, check, wrong, a, 1); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("cold launch folded Hibernate wake hold: %v", err)
	}
	if err := f.db.RecordAccessWakeAdmission(ctx, check, have, a, 1); err != nil {
		t.Fatal(err)
	}
	checks, err = f.db.ListAccessWakeChecks(ctx, "a")
	if err != nil || len(checks) != 0 {
		t.Fatalf("covered WakeDemand still pending: %+v, %v", checks, err)
	}
	req.Placement.Resources.MemoryMiB = 1024
	if _, _, err := f.db.Accept(ctx, req); err != nil {
		t.Fatalf("same-session covered Restore charged twice: %v", err)
	}
}

func TestWorkerPolicyOverlayUsesCurrentDesiredRevisionOnReactivation(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	savePlacementSample(t, f.db, a)
	b := f.hibernatedBinding(t, "a")
	w, err := f.db.GetWorker(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	cordon, err := f.db.SetWorkerMaintenance(ctx, "a", w.Revision, "cordoned", "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := f.db.SetWorkerMaintenance(ctx, "a", cordon.Revision, "draining", "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	overlay, managed, err := f.db.WorkerPolicyPlan(ctx, b)
	if err != nil || !managed || overlay.Effective.Wake != (rt.WakeConditions{}) ||
		!overlay.Effective.Activity.Leases || !overlay.Effective.Activity.Tasks {
		t.Fatalf("drain overlay = %+v, managed=%t, err=%v", overlay, managed, err)
	}
	if _, err := f.db.RecordWorkerPolicyOverlay(ctx, overlay, 5); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"keep_running":true,"wake_conditions":{"allow_cold_start":true,"api_access":false}}`)
	if _, err := f.db.SetLifecycleDesired(ctx, b.MachineID, "user-policy", "override", overlay.DesiredRevision, 1, patch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordWorkerPolicyOverlay(ctx, overlay, 5); !errors.Is(err, registry.ErrRevision) {
		t.Fatalf("late overlay superseded current desired policy: %v", err)
	}
	active, err := f.db.SetWorkerMaintenance(ctx, "a", drain.Revision, "active", "maintenance complete")
	if err != nil || active.Phase != "activating" {
		t.Fatalf("desired active = %+v, %v", active, err)
	}
	activePlan, managed, err := f.db.WorkerPolicyPlan(ctx, b)
	if err != nil || !managed || activePlan.DesiredRevision != overlay.DesiredRevision+1 ||
		!activePlan.Effective.KeepRunning || !activePlan.Effective.Wake.ColdStart || activePlan.Effective.Wake.API {
		t.Fatalf("reactivation plan = %+v, %v", activePlan, err)
	}
	result, err := f.db.RecordWorkerPolicyOverlay(ctx, activePlan, 6)
	if err != nil || result.Applied == nil || *result.Applied != result.Desired {
		t.Fatalf("reactivated policy = %+v, %v", result, err)
	}
}
