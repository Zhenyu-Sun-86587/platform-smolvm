package registry_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func unlockPlacementSource(t *testing.T, f placementFixture, binding registry.Binding) {
	t.Helper()
	if _, err := f.db.DB().ExecContext(context.Background(), "DELETE FROM operation_locks WHERE machine_id=?", binding.MachineID); err != nil {
		t.Fatal(err)
	}
}

func TestManualRestoreUsesCapturedResourcesOnOriginalReader(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	a.Profiles[0].Capabilities = append(a.Profiles[0].Capabilities, rt.FullStateOperationCapability(rt.VMRestore))
	a.Budget.Capacity.StorageBytes = 8 << 30
	a.Budget.Allocated.StorageBytes = 4 << 30
	savePlacementSample(t, f.db, a)
	b := f.worker(t, "b")
	b.Profiles[0].Capabilities = append(b.Profiles[0].Capabilities, rt.FullStateOperationCapability(rt.VMRestore))
	savePlacementSample(t, f.db, b)
	source := f.hibernatedBinding(t, "a")
	unlockPlacementSource(t, f, source)
	req := f.request("capture")
	req.Placement = nil
	req.Kind, req.MachineID, req.ExpectedGeneration = rt.VMSnapshot, source.MachineID, 1
	capture, _, err := f.db.Accept(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"boot_mode":"system","resources":{"cpu_count":2,"memory_mib":3072}}`)
	digest := fmt.Sprintf("sha256:%064d", 1)
	manifest, err := json.Marshal(map[string]any{
		"captured_configuration": map[string]any{"status": "verified", "schema_version": 1, "source_machine_revision": 1, "capture_id": "point",
			"manifest_digest": digest, "spec_digest": fmt.Sprintf("sha256:%x", sha256.Sum256(spec)), "spec": spec},
		"captured_scope": map[string]any{"capture_id": "point", "manifest_digest": digest, "disks": []map[string]any{
			{"device_id": "root", "role": "root", "generation_id": "root-generation", "read_only": false, "capacity_bytes": 3 << 30},
			{"device_id": "data", "role": "data", "generation_id": "data-generation", "read_only": false, "capacity_bytes": 3 << 30},
			{"device_id": "shared", "role": "data", "generation_id": "shared-generation", "read_only": true, "capacity_bytes": 8 << 30},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,label,task_content,
		state,relative_path,sha256,manifest_json,provenance,closure_verified_at,created_at) VALUES('snapshot',?,1,'recovery','','unknown','ready',
		'snapshot',?,'{}','worker',1,1)`, source.MachineID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `INSERT INTO managed_statepoint_receipts VALUES
		('global-a','point','representation','bundle',?,?,'checkpoint',?,'{}')`, source.ID, capture.ID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `INSERT INTO vm_snapshot_provenance VALUES
		('snapshot',?,'global-a','bundle','reader','point')`, capture.ID); err != nil {
		t.Fatal(err)
	}
	restore := f.request("restore")
	restore.Placement = nil
	restore.Kind, restore.RuntimeInstanceID, restore.DesiredPower = rt.VMRestore, "global-a", "running"
	restore.Request, restore.MachineSpec = []byte(`{"snapshot_id":"snapshot"}`), spec
	restore.Heavy = true
	if _, _, err := f.db.Accept(ctx, restore); !errors.Is(err, placement.ErrNoCandidate) || exclusionReason(err, "a") != "statepoint_resource_evidence_unavailable" {
		t.Fatalf("unrecorded capture admitted: %v", err)
	}
	if _, err := f.db.DB().ExecContext(ctx, "UPDATE snapshots SET manifest_json=? WHERE id='snapshot'", string(manifest)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Accept(ctx, restore); !errors.Is(err, placement.ErrNoCandidate) || exclusionReason(err, "a") != "budget_insufficient" {
		t.Fatalf("restore used a different Worker or bypassed disk budget: %v", err)
	}
	a.Budget.Allocated.StorageBytes = 0
	a.Sequence, a.QueryNonce = 2, "restore-ready"
	savePlacementSample(t, f.db, a)
	op, _, err := f.db.Accept(ctx, restore)
	if err != nil || op.WorkerID != "a" {
		t.Fatalf("restore placement: %+v, %v", op, err)
	}
	r, err := f.db.GetPlacementReservation(ctx, op.ID)
	if err != nil || r.Resources.VCPUs != 2 || r.Resources.MemoryMiB != 3072 || r.Resources.StorageBytes != 6<<30 {
		t.Fatalf("restore resource vector = %+v, %v", r, err)
	}
	statepointIntent := []byte(`{"statepoint":{"kind":"vm_restore","request_digest":"sha256:restore"}}`)
	if _, err := f.db.Transition(ctx, op.ID, registry.TransitionRequest{State: registry.Dispatching, Stage: "worker_restore", Dispatch: statepointIntent}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Transition(ctx, op.ID, registry.TransitionRequest{State: registry.Observing, Stage: "worker_restore", Effect: registry.EffectUnknown}); err != nil {
		t.Fatal(err)
	}
	proof := placement.Admission{OperationID: op.ID, Attempt: 1, WorkerID: "a", RuntimeInstanceID: "global-a", BundleID: "bundle",
		ControlEpoch: f.epoch, EnrollmentGeneration: 1, Session: a.Session, NativeDigest: "sha256:restore", Kind: placement.AdmissionRestore,
		ReceiptID: op.ID, ConfirmedThroughSequence: a.Sequence}
	if err := f.db.RecordPlacementAdmission(ctx, proof); err != nil {
		t.Fatal(err)
	}
	a.Sequence, a.QueryNonce = 3, "restore-covered"
	savePlacementSample(t, f.db, a)
	r, err = f.db.GetPlacementReservation(ctx, op.ID)
	if err != nil || r.Phase != placement.Covered {
		t.Fatalf("restore hold did not cover: %+v, %v", r, err)
	}
}

func exclusionReason(err error, worker string) string {
	var rejected *placement.Rejections
	if errors.As(err, &rejected) {
		for _, candidate := range rejected.Candidates {
			if candidate.WorkerID == worker {
				return candidate.Reason
			}
		}
	}
	return ""
}

func TestForkBatchAndRetryChargeOriginalWritableCapacity(t *testing.T) {
	f := placementDB(t)
	ctx := context.Background()
	a := f.worker(t, "a")
	a.Profiles[0].Capabilities = append(a.Profiles[0].Capabilities, rt.FullStateOperationCapability(rt.ForkBatch))
	a.Budget.Capacity.StorageBytes = 5 << 30
	a.Budget.Capacity.MemoryMiB = 24 * 1024
	a.Pressure.AvailableMemoryMiB = 24 * 1024
	savePlacementSample(t, f.db, a)
	source := f.hibernatedBinding(t, "a")
	unlockPlacementSource(t, f, source)
	req := f.request("batch")
	req.Placement = nil
	req.Kind, req.MachineID, req.ExpectedGeneration = rt.ForkBatch, source.MachineID, 1
	req.ForkChildren = []string{"child-a", "child-b"}
	req.Heavy = true
	req.AdmitNative = func(registry.Binding, string) error { return nil }
	if _, _, err := f.db.Accept(ctx, req); !errors.Is(err, placement.ErrNoCandidate) || exclusionReason(err, "a") != "fork_storage_evidence_unavailable" {
		t.Fatalf("fork without device proof = %v", err)
	}
	evidence := &rt.ForkStorageEvidence{Execution: source.ExecutionRef(), HostBootID: a.Session.HostBootID, WritableBytes: 3 << 30}
	epoch := source.ExecutionEpoch
	observation := registry.Observation{ObservedState: "running", Availability: "blocked", NativeState: "running", ExpectedExecutionEpoch: &epoch,
		Native: &registry.NativeObservation{Execution: source.ExecutionRef(), PowerState: registry.PowerRunning, ForkStorage: evidence}}
	if err := f.db.ObserveMachine(ctx, source.ID, observation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Accept(ctx, req); !errors.Is(err, placement.ErrNoCandidate) || exclusionReason(err, "a") != "budget_insufficient" {
		t.Fatalf("batch writable budget ignored: %v", err)
	}
	a.Budget.Capacity.StorageBytes = 8 << 30
	a.Sequence, a.QueryNonce = 2, "fork-room"
	savePlacementSample(t, f.db, a)
	op, _, err := f.db.Accept(ctx, req)
	if err != nil || op.WorkerID != "a" || len(op.Children) != 2 {
		t.Fatalf("fork placement = %+v, %v", op, err)
	}
	r, err := f.db.GetPlacementReservation(ctx, op.ID)
	if err != nil || r.Resources.StorageBytes != 6<<30 {
		t.Fatalf("aggregate fork storage = %+v, %v", r, err)
	}
	evidence.WritableBytes = 4 << 30
	var changed *rt.Error
	if err := f.db.VerifyForkPlacementStorage(ctx, op.ID, 2, evidence); !errors.As(err, &changed) || changed.Code != "FORK_STORAGE_EVIDENCE_CHANGED" {
		t.Fatalf("device change before dispatch = %v", err)
	}
	// The source can change or disappear after its capture has been released.
	// The next attempt must use the original child ledger vector.
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE operations SET state='succeeded',stage='completed',effect='partial',source_lock_released=1 WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE operation_children SET state='quarantined',error_code='NATIVE_CHILD_ABORTED'
		WHERE operation_id=? AND ordinal=1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE placement_reservations SET phase='covered' WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE machines SET spec_json='{"resources":{"cpu_count":8,"memory_mib":16000,"storage_gib":99}}' WHERE id=?`, source.MachineID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := f.db.BeginNativeForkRetryTx(ctx, tx, op.ID, op.Version, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, err = f.db.GetPlacementReservation(ctx, op.ID)
	if err != nil || r.NativeAttempt != 1 || r.Attempt != 2 || r.Resources.StorageBytes != 3<<30 || r.Resources.MemoryMiB != 6144 || r.Resources.VCPUs != 2 {
		t.Fatalf("retry used current source resources: %+v, %v", r, err)
	}
}
