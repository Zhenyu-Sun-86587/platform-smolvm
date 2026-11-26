package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

type managedSSHTunnelRuntime struct {
	rt.Adapter
	rt.NativeGuestControl
}

func (managedSSHTunnelRuntime) LifecycleWorkerSocket() string { return "/unused-managed-ssh-test.sock" }

func managedSSHTunnelFixture(t *testing.T) (*Server, auth.Principal) {
	t.Helper()
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conf := config.Defaults()
	conf.API.PublicOrigin = "https://sandbox.test"
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	runtimes := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route: managedSSHTunnelRuntime{}})
	s := New(lifecycle.New(db, runtimes, conf), &snapshots.Catalog{DB: db}, nil)
	owner, err := s.Auth.Bootstrap(ctx, "owner", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.EnsureTemplate(ctx, registry.Template{ID: "system", Name: "System", Version: 1,
		ImageDigest: "sha256:" + strings.Repeat("1", 64), Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"root","managed_ssh":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err = db.RegisterRuntime(ctx, route, "test-socket", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB().ExecContext(ctx, `INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('m_managed_ssh','Machine','system',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,
			runtime_instance_id,runtime_bundle_id,runtime_instance_object_id,execution_epoch,incarnation_id,power_state)
		VALUES ('binding','m_managed_ssh',1,'vm','current','task-ns','digest','serve','bundle','native-vm',4,'vmm-4','hibernated');
		UPDATE machines SET current_binding_id='binding' WHERE id='m_managed_ssh'`); err != nil {
		t.Fatal(err)
	}
	return s, owner
}

func managedSSHTunnelToken(t *testing.T, s *Server, owner auth.Principal, label, scope string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, token, err := s.Auth.IssueTokenTx(ctx, tx, owner, label, auth.Scope{Access: scope, MachineID: "m_managed_ssh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestManagedSSHTunnelIsExplicitScopedAndIndependentOfPublications(t *testing.T) {
	s, owner := managedSSHTunnelFixture(t)
	manage := managedSSHTunnelToken(t, s, owner, "manage", auth.AccessManage)
	tunnel := managedSSHTunnelToken(t, s, owner, "tunnel", auth.AccessTunnel)
	const path = "/v1/machines/m_managed_ssh/ssh-tunnel"
	call := func(query, bearer string, upgrade bool, status int, reason string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path+query, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if upgrade {
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Sec-WebSocket-Protocol", managedSSHProtocol)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != status || reason != "" && !strings.Contains(w.Body.String(), reason) {
			t.Fatalf("%s gave %d %s, want %d %s", query, w.Code, w.Body.String(), status, reason)
		}
	}
	call("?managed=1", "", true, http.StatusUnauthorized, "UNAUTHENTICATED")
	call("?managed=1", tunnel, true, http.StatusForbidden, "PERMISSION_DENIED")
	call("?managed=1&managed=0", manage, true, http.StatusBadRequest, "INVALID_ARGUMENT")
	call("?managed=1", manage, true, http.StatusConflict, "MANAGED_SSH_PRIVATE_NETWORK_REQUIRED")
	call("", tunnel, false, http.StatusNotFound, "NOT_FOUND")

	ctx := context.Background()
	if _, err := s.DB.DB().ExecContext(ctx, `INSERT INTO services(id,machine_id,name,revision,kind,guest_port,readiness_json,access_json,enabled)
		VALUES ('published','m_managed_ssh','Published SSH',1,'ssh',2201,'{}','{"private":true}',1)`); err != nil {
		t.Fatal(err)
	}
	call("", tunnel, false, http.StatusBadRequest, "INVALID_ARGUMENT")
	call("?managed=1", tunnel, true, http.StatusForbidden, "PERMISSION_DENIED")
	call("?managed=1", manage, true, http.StatusConflict, "MANAGED_SSH_PRIVATE_NETWORK_REQUIRED")
	if _, err := s.DB.DB().ExecContext(ctx, "UPDATE services SET enabled=0,revision=2 WHERE id='published'"); err != nil {
		t.Fatal(err)
	}
	call("", tunnel, false, http.StatusNotFound, "NOT_FOUND")
	call("?managed=1", manage, true, http.StatusConflict, "MANAGED_SSH_PRIVATE_NETWORK_REQUIRED")

	// A non-managed profile fails even if an SSH service happens to be present.
	if _, err := s.DB.DB().ExecContext(ctx, `UPDATE templates SET spec_json=json_remove(spec_json,'$.managed_ssh') WHERE id='system'`); err != nil {
		t.Fatal(err)
	}
	call("?managed=1", manage, true, http.StatusConflict, "MANAGED_SSH_PROFILE_REQUIRED")
	var count int
	if err := s.DB.ReadDB().QueryRowContext(ctx, "SELECT count(*) FROM operations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("ineligible or unauthorized access dispatched an operation: %d, %v", count, err)
	}
}

func TestManagedSSHStreamRejectsChangedExecutionAndRevokedCredential(t *testing.T) {
	s, owner := managedSSHTunnelFixture(t)
	ctx := context.Background()
	bearer := managedSSHTunnelToken(t, s, owner, "stream", auth.AccessManage)
	p, err := s.Auth.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.DB().ExecContext(ctx, "UPDATE runtime_bindings SET power_state='running' WHERE id='binding'"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ManagedSystemSSHBinding(ctx, "m_managed_ssh")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.checkManagedSSHExecution(ctx, p, before, "unverified host key"); !errors.Is(err, registry.ErrGeneration) {
		t.Fatalf("unverified SSH evidence admitted: %v", err)
	}
	if _, err = s.DB.DB().ExecContext(ctx, "UPDATE runtime_bindings SET execution_epoch=5,incarnation_id='vmm-5' WHERE id='binding'"); err != nil {
		t.Fatal(err)
	}
	newBinding, err := s.currentManagedSSHBinding(ctx, before)
	if err != nil || newBinding.ExecutionEpoch != 5 {
		t.Fatalf("binding-scope check never observed the next epoch: %+v, %v", newBinding.ExecutionRef(), err)
	}
	if err = s.checkManagedSSHExecution(ctx, p, before, "old host key"); !errors.Is(err, registry.ErrGeneration) {
		t.Fatalf("changed incarnation admitted into original tunnel: %v", err)
	}
	if _, err = s.DB.DB().ExecContext(ctx, "UPDATE credentials SET revoked_at=? WHERE id=?", time.Now().UnixMilli(), p.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err = s.checkManagedSSHExecution(ctx, p, newBinding, "key"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked credential survived tunnel reauthorization: %v", err)
	}
}
