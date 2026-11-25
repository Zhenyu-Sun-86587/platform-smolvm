package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func TestRevokedGuestCreateNeverReachesNativeLaunch(t *testing.T) {
	ctx := context.Background()
	official := &createRuntime{}
	engine, ownerID, epoch := runtimeTestEngine(t, ctx, official)
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	worker := &systemLaunchRuntime{acceptFirst: true}
	worker.route = route
	engine.runtimes = rt.NewResolver(official, map[rt.BundleRoute]rt.Adapter{route: worker})
	if err := engine.DB.RegisterRuntime(ctx, route, "system-socket", JSON(map[string]string{"schema": "RuntimeBundle/1"})); err != nil {
		t.Fatal(err)
	}
	template := registry.Template{ID: "t_system", Name: "System", Version: 1,
		ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Spec:        json.RawMessage(`{"boot_mode":"system","root_generation_id":"debian-root"}`)}
	if err := engine.DB.EnsureTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.DB.DB().ExecContext(ctx, `INSERT INTO machines
		(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('m_source','source','t_system',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings (id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,
			runtime_instance_id,runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
		VALUES ('source-bind','m_source',1,'source','current','tasks-source','digest','serve','bundle','source-vm','source-1',1,'running');
		UPDATE machines SET current_binding_id='source-bind' WHERE id='m_source'`); err != nil {
		t.Fatal(err)
	}
	peer := auth.AttestedGuestContext{MachineID: "m_source", BindingID: "source-bind", RuntimeInstanceID: "serve",
		RuntimeBundleID: "bundle", RuntimeObjectID: "source-vm", IncarnationID: "source-1", ExecutionEpoch: 1}
	owner := auth.Principal{ID: ownerID, Scope: auth.Scope{Access: auth.AccessManage}}
	authority := auth.New(engine.DB)
	grant, err := authority.CreateGuestGrant(ctx, owner, auth.GuestGrantSpec{SourceMachineID: peer.MachineID,
		Action: auth.GuestCreate, ExpiresAt: time.Now().Add(time.Minute), MaximumUses: 1,
		Limits: auth.GuestLimits{MaximumChildren: 1, MaximumMemoryMiB: 512,
			Create: &auth.GuestCreateProfile{TemplateID: template.ID, RuntimeInstanceID: route.InstanceID, CPUs: 1, MemoryMiB: 512}}})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := authority.IssueGuestCredential(ctx, owner, peer, grant, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := authority.AuthenticateGuest(ctx, peer, secret, auth.GuestCreate, "")
	if err != nil {
		t.Fatal(err)
	}
	request := Create{Name: "delegated", TemplateID: template.ID, RuntimeInstanceID: route.InstanceID, BootMode: rt.BootSystem, Start: true}
	request.Resources.CPUs, request.Resources.Memory = 1, 512
	accepted, _, err := engine.DB.Accept(ctx, registry.AcceptRequest{Kind: "create", PrincipalID: guest.ID,
		Epoch: epoch, Key: "one", Scope: "POST /v1/machines", Name: request.Name, TemplateID: request.TemplateID,
		RuntimeInstanceID: route.InstanceID, Request: JSON(request), Heavy: true,
		AdmitRuntime: func(rt.BundleRoute, string) error { return nil },
		AdmitGuest: func(ctx context.Context, tx *sql.Tx, op registry.Operation, replay bool) error {
			return authority.AuthorizeGuestOperation(ctx, tx, peer, secret, guest, op, replay, 512)
		}})
	if err != nil || accepted.State != registry.Queued {
		t.Fatal(accepted, err)
	}
	if err := engine.DB.CheckGuestDispatch(ctx, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if err := authority.RevokeGuestGrant(ctx, owner, grant, 1); err != nil {
		t.Fatal(err)
	}
	if err := engine.DB.CheckGuestDispatch(ctx, accepted.ID); !errors.Is(err, registry.ErrGuestAuthorityRevoked) {
		t.Fatalf("queued authority after revocation = %v", err)
	}
	if err := engine.drive(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	finished, err := engine.DB.GetOperation(ctx, accepted.ID)
	if err != nil || finished.State != registry.Failed || finished.Effect != registry.EffectNone ||
		len(worker.requests) != 0 || len(official.creates) != 0 {
		t.Fatalf("revoked create reached a runtime: %+v, %v", finished, err)
	}
}
