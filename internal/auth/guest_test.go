package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/registry"
)

func TestGuestGrantRequiresAttestedCurrentExecution(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := New(store)
	owner, err := a.Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO templates
		(id,name,version,image_digest,spec_json,capabilities_json) VALUES ('t','base',1,'image','{}','{}');
		INSERT INTO runtime_bundles VALUES ('bundle','manifest','{}','verified','ready',1);
		INSERT INTO runtime_instances(id,lane,runtime_bundle_id,endpoint_key,status,revision,worker_id,local_instance_id,worker_endpoint_key)
		VALUES ('serve','deep','bundle','worker/local-worker/runtime/serve','ready',1,'local-worker','serve','endpoint');
		INSERT INTO machines (id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('m_source','source','t',1,1,'running','{}','{}',0), ('m_child','child','t',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings (id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,
			runtime_instance_id,runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
		VALUES ('source-bind','m_source',1,'source','current','tasks-source','digest','serve','bundle','source-vm','source-1',1,'running'),
			('child-bind','m_child',1,'child','current','tasks-child','digest','serve','bundle','child-vm','child-1',1,'running');
		UPDATE machines SET current_binding_id=CASE id WHEN 'm_source' THEN 'source-bind' ELSE 'child-bind' END;`); err != nil {
		t.Fatal(err)
	}
	source := AttestedGuestContext{
		MachineID: "m_source", BindingID: "source-bind", RuntimeInstanceID: "serve",
		RuntimeBundleID: "bundle", RuntimeObjectID: "source-vm", IncarnationID: "source-1", ExecutionEpoch: 1,
	}
	clone := AttestedGuestContext{
		MachineID: "m_child", BindingID: "child-bind", RuntimeInstanceID: "serve",
		RuntimeBundleID: "bundle", RuntimeObjectID: "child-vm", IncarnationID: "child-1", ExecutionEpoch: 1,
	}
	grant, err := a.CreateGuestGrant(ctx, owner, GuestGrantSpec{
		SourceMachineID: source.MachineID, Action: GuestExec, TargetMachineID: clone.MachineID,
		ExpiresAt: time.Now().Add(time.Hour), MaximumUses: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.IssueGuestCredential(ctx, owner, source, grant, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("public bearer accepted: %v", err)
	}
	var forged AttestedGuestContext
	if err := json.Unmarshal([]byte(`{"MachineID":"m_source","BindingID":"source-bind","IncarnationID":"source-1","ExecutionEpoch":1}`), &forged); err != nil || forged.valid() {
		t.Fatal("Guest JSON became a host context", err)
	}
	accept := func(_ *sql.Tx, p GuestPrincipal) error {
		if p.ID == owner.ID || p.ID == "" || p.GrantID != grant || p.Action != GuestExec || p.Target != clone.MachineID {
			t.Fatalf("wrong Guest principal: %+v", p)
		}
		return nil
	}
	if err := a.UseGuestGrant(ctx, clone, secret, GuestExec, clone.MachineID, accept); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("copied Guest credential accepted: %v", err)
	}
	if err := a.UseGuestGrant(ctx, source, secret, GuestRead, clone.MachineID, accept); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a different action was allowed: %v", err)
	}
	failed := errors.New("test admission rolled back")
	if err := a.UseGuestGrant(ctx, source, secret, GuestExec, clone.MachineID,
		func(*sql.Tx, GuestPrincipal) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.UseGuestGrant(ctx, source, secret, GuestExec, clone.MachineID, accept); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.UseGuestGrant(ctx, source, secret, GuestExec, clone.MachineID, accept); !errors.Is(err, ErrForbidden) {
		t.Fatalf("grant exceeded quota: %v", err)
	}
	if err := a.RevokeGuestGrant(ctx, owner, grant, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.UseGuestGrant(ctx, source, secret, GuestExec, clone.MachineID, accept); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked grant accepted: %v", err)
	}
}
