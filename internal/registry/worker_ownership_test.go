package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

func TestWorkerMigrationPreservesPinnedIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "platform.db")
	old := legacyRegistry(t, path, 1019)
	_, err := old.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,1019,'install','control');
		INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES('tpl','base',1,'image','{}','{}');
		INSERT INTO principals(id,username,created_at) VALUES('owner','owner',1);
		INSERT INTO runtime_bundles VALUES('bundle','bundle','{"contracts":{"state_reader_id":"reader"}}','verified','active',1);
		INSERT INTO runtime_instances VALUES('serve','deep','bundle','/run/test/serve.sock','registered',5);
		INSERT INTO runtime_instances VALUES('serve-other','deep','bundle','worker/local-worker/runtime/serve','registered',1);
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES('vm','old-machine','tpl',3,7,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,runtime_bundle_id,
			runtime_instance_object_id,incarnation_id,execution_epoch)
		VALUES('bind','vm',7,'old-name','current','old-ns','old-spec','serve','bundle','managed','inc',11);
		UPDATE machines SET current_binding_id='bind' WHERE id='vm';
		INSERT INTO operations(id,control_epoch,principal_id,machine_id,binding_id,binding_generation,runtime_name,kind,
			state,stage,request_json,dispatch_json,effect,created_at,updated_at,heavy)
		VALUES('op','control','owner','vm','bind',7,'old-name','vm_snapshot','succeeded','done','{}',
			'{"statepoint":{"request_digest":"prior-digest"}}','applied',1,2,0);
		INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,task_content,state,relative_path,provenance,created_at)
		VALUES('snapshot','vm',7,'recovery','unknown','ready','snapshot','local_record',2);
		INSERT INTO vm_snapshot_provenance VALUES('snapshot','op','serve','bundle','reader','capture');
		INSERT INTO managed_statepoint_receipts VALUES('serve','capture','representation','bundle','bind','op','checkpoint','old-digest','{}');
		INSERT INTO volumes VALUES('volume','owner','old-volume','serve','bundle',1,'available','{}',1);
		INSERT INTO volume_attachments VALUES('attachment','volume','vm','bind','reserved',1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	local := rt.BundleRoute{WorkerID: rt.LocalWorkerID, InstanceID: "serve", LocalInstanceID: "serve", BundleID: "bundle"}
	profile, err := store.GetRuntimeInstance(ctx, local.InstanceID)
	if err != nil || profile.BundleRoute() != local || profile.Revision != 5 || profile.EndpointKey != "worker/local-worker/runtime/serve" {
		t.Fatal(profile, err)
	}
	manifest := json.RawMessage(`{"contracts":{"state_reader_id":"reader"}}`)
	if err := store.RegisterRuntime(ctx, local, "/run/test/serve.sock", manifest); err != nil {
		t.Fatal(err)
	}
	remote := rt.BundleRoute{WorkerID: "remote", InstanceID: "global-serve", LocalInstanceID: "serve", BundleID: "bundle"}
	if err := store.RegisterWorker(ctx, registry.Worker{ID: remote.WorkerID, Name: "Remote"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterRuntime(ctx, remote, "/run/test/serve.sock", manifest); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetRuntimeRoute(ctx, remote.InstanceID); err != nil || got != remote {
		t.Fatal(got, err)
	}
	duplicate := remote
	duplicate.InstanceID = "global-duplicate"
	if err := store.RegisterRuntime(ctx, duplicate, "/run/test/other.sock", manifest); !errors.Is(err, registry.ErrConflict) {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.Control(ctx)
	if err != nil || control.SchemaVersion < 1020 || control.ControlEpoch != "control" || control.InstallationID != "install" {
		t.Fatal(control, err)
	}
	machine, err := store.GetMachine(ctx, "vm")
	if err != nil || machine.WorkerID != local.WorkerID || machine.Revision != 3 || machine.BindingGeneration != 7 ||
		machine.CurrentBinding == nil || machine.CurrentBinding.ID != "bind" ||
		machine.CurrentBinding.BundleRoute() != local || machine.CurrentBinding.ExecutionEpoch != 11 {
		t.Fatal(machine, err)
	}
	op, err := store.GetOperation(ctx, "op")
	if err != nil || op.BundleRoute() != local || op.BindingID != "bind" ||
		string(op.Dispatch) != `{"statepoint":{"request_digest":"prior-digest"}}` {
		t.Fatal(op, err)
	}
	snapshot, err := (&snapshots.Catalog{DB: store}).Get(ctx, "snapshot")
	if err != nil || snapshot.VM == nil || snapshot.VM.WorkerID != local.WorkerID || snapshot.VM.RuntimeCaptureID != "capture" ||
		snapshot.VM.StateReaderID != "reader" || snapshot.VM.RuntimeInstanceID != local.InstanceID || snapshot.VM.OperationID != "op" {
		t.Fatal(snapshot, err)
	}
	var representation, operation, binding, volume string
	err = store.ReadDB().QueryRowContext(ctx, `SELECT p.representation_id,p.operation_id,p.binding_id,a.volume_id
		FROM managed_statepoint_receipts p JOIN volume_attachments a ON a.binding_id=p.binding_id WHERE p.capture_id='capture'`).
		Scan(&representation, &operation, &binding, &volume)
	if err != nil || representation != "representation" || operation != "op" || binding != "bind" || volume != "volume" {
		t.Fatal(representation, operation, binding, volume, err)
	}
	rows, err := store.ReadDB().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("migration changed existing foreign keys", rows.Err())
	}
}

func TestWorkerPinsRejectCrossNodeResources(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := auth.New(store).Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureTemplate(ctx, registry.Template{ID: "tpl", Name: "base", Version: 1, ImageDigest: "image"}); err != nil {
		t.Fatal(err)
	}
	local := rt.BundleRoute{WorkerID: rt.LocalWorkerID, InstanceID: "runtime-a", BundleID: "bundle-a"}
	remote := rt.BundleRoute{WorkerID: "remote", InstanceID: "runtime-b", LocalInstanceID: "runtime-a", BundleID: "bundle-b"}
	if err := store.RegisterWorker(ctx, registry.Worker{ID: remote.WorkerID, Name: "Remote"}); err != nil {
		t.Fatal(err)
	}
	manifest := json.RawMessage(`{"contracts":{"state_reader_id":"reader"}}`)
	for _, route := range []rt.BundleRoute{local, remote} {
		if err := store.RegisterRuntime(ctx, route, "/run/test/serve.sock", manifest); err != nil {
			t.Fatal(err)
		}
	}
	db := store.DB()
	_, err = db.ExecContext(ctx, `INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running,worker_id)
		VALUES('vm-a','vm-a','tpl',1,1,'running','{}','{}',0,?),('vm-b','vm-b','tpl',1,1,'running','{}','{}',0,?);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,worker_id,runtime_instance_id,runtime_bundle_id)
		VALUES('bind-a','vm-a',1,'vm-a','current','ns-a','spec-a','local-worker','runtime-a','bundle-a'),
			('bind-b','vm-b',1,'vm-b','current','ns-b','spec-b','remote','runtime-b','bundle-b');
		UPDATE machines SET current_binding_id='bind-a' WHERE id='vm-a';
		UPDATE machines SET current_binding_id='bind-b' WHERE id='vm-b';
		 INSERT INTO operations(id,control_epoch,machine_id,binding_id,binding_generation,kind,state,stage,request_json,effect,created_at,updated_at,heavy,worker_id,runtime_instance_id,runtime_bundle_id)
		  VALUES('capture-op',?,'vm-a','bind-a',1,'vm_snapshot','succeeded','done','{}','applied',1,2,0,'local-worker','runtime-a','bundle-a');
		INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,task_content,state,relative_path,provenance,created_at)
		  VALUES('snapshot','vm-a',1,'recovery','unknown','ready','snapshot','local_record',2);
		INSERT INTO vm_snapshot_provenance VALUES('snapshot','capture-op','runtime-a','bundle-a','reader','capture');
		INSERT INTO volumes VALUES('vol',?,'volume','runtime-a','bundle-a',1,'available','{}',1);`,
		local.WorkerID, remote.WorkerID, control.ControlEpoch, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO volume_attachments VALUES('attach','vol','vm-b','bind-b','reserved',1)`,
		`UPDATE volumes SET runtime_instance_id='runtime-b',runtime_bundle_id='bundle-b' WHERE id='vol'`,
		`INSERT INTO managed_statepoint_receipts VALUES('runtime-b','capture','representation','bundle-b','bind-a','capture-op','checkpoint','digest','{}')`,
		`UPDATE operations SET worker_id='remote',runtime_instance_id='runtime-b',runtime_bundle_id='bundle-b' WHERE id='capture-op'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("cross-Worker write succeeded: %s", statement)
		}
	}
	attach := registry.AcceptRequest{Kind: rt.VolumeOperation, PrincipalID: owner.ID, Epoch: control.ControlEpoch,
		Scope: "POST /v1/machines/vm-b/volume-attachments", Key: "attach", MachineID: "vm-b",
		ExpectedGeneration: 1, Request: json.RawMessage(`{"runtime_instance_id":"runtime-a","runtime_bundle_id":"bundle-a"}`),
		AdmitRuntime: func(rt.BundleRoute, string) error { t.Fatal("cross-Worker attachment reached admission"); return nil }}
	if _, _, err := store.Accept(ctx, attach); !errors.Is(err, registry.ErrConflict) {
		t.Fatal(err)
	}
	req := registry.AcceptRequest{Kind: rt.VMRestore, PrincipalID: owner.ID, Epoch: control.ControlEpoch,
		Scope: "POST /v1/restores", Key: "restore", Name: "target", TemplateID: "tpl",
		RuntimeInstanceID: remote.InstanceID, Request: json.RawMessage(`{"snapshot_id":"snapshot"}`),
		AdmitRuntime: func(rt.BundleRoute, string) error { t.Fatal("cross-Worker restore reached admission"); return nil }}
	if _, _, err := store.Accept(ctx, req); !errors.Is(err, registry.ErrConflict) {
		t.Fatal(err)
	}
	req.RuntimeInstanceID = local.InstanceID
	req.WorkerID = remote.WorkerID
	if _, _, err := store.Accept(ctx, req); !errors.Is(err, registry.ErrConflict) {
		t.Fatal(err)
	}
	req.WorkerID = local.WorkerID
	req.AdmitRuntime = func(route rt.BundleRoute, _ string) error {
		if route.Key() != local.Key() {
			t.Fatal(route)
		}
		return nil
	}
	op, replay, err := store.Accept(ctx, req)
	if err != nil || replay || op.BundleRoute().Key() != local.Key() {
		t.Fatal(op, replay, err)
	}
}

func TestWorkerEnrollmentIsUniqueAndRotationIsAtomic(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err := store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := registry.WorkerEnrollment{WorkerID: "worker-a", Generation: 1, Authority: control.ControlEpoch, CertificateFingerprint: "cert-a"}
	second := registry.WorkerEnrollment{WorkerID: "worker-b", Generation: 1, Authority: control.ControlEpoch, CertificateFingerprint: "cert-b"}
	for _, enrollment := range []registry.WorkerEnrollment{first, second} {
		if err := store.RegisterWorker(ctx, registry.Worker{ID: enrollment.WorkerID, Name: enrollment.WorkerID}); err != nil {
			t.Fatal(err)
		}
		if err := store.EnrollWorker(ctx, enrollment); err != nil {
			t.Fatal(err)
		}
	}
	changed := first
	changed.Generation++
	changed.CertificateFingerprint = second.CertificateFingerprint
	if err := store.EnrollWorker(ctx, changed); !errors.Is(err, registry.ErrConflict) {
		t.Fatal(err)
	}
	if got, err := store.GetWorkerEnrollment(ctx, first.WorkerID); err != nil || got.Generation != first.Generation || got.CertificateFingerprint != first.CertificateFingerprint {
		t.Fatal(got, err)
	}
	changed.CertificateFingerprint = "cert-a-new"
	if err := store.EnrollWorker(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollWorker(ctx, changed); err != nil {
		t.Fatal("replay", err)
	}
	if err := store.EnrollWorker(ctx, first); !errors.Is(err, registry.ErrConflict) {
		t.Fatal("old generation", err)
	}
	if got, err := store.GetWorkerEnrollment(ctx, changed.WorkerID); err != nil || got.Generation != changed.Generation || got.CertificateFingerprint != changed.CertificateFingerprint {
		t.Fatal(got, err)
	}
}
