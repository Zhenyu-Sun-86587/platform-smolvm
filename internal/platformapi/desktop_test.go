package platformapi

import (
	"context"
	"encoding/json"
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

type historicalDesktopRuntime struct{ rt.Adapter }

func TestDesktopSessionFencesCredentialsAndExpiresLease(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctl, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conf := config.Defaults()
	conf.API.PublicOrigin = "https://sandbox.test"
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	runtimes := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route: historicalDesktopRuntime{}})
	s := New(lifecycle.New(db, runtimes, conf), &snapshots.Catalog{DB: db}, nil)
	p, err := s.Auth.Bootstrap(ctx, "owner", "a-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token := func(label string) string {
		t.Helper()
		tx, err := db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, token, err := s.Auth.IssueTokenTx(ctx, tx, p, label, auth.Scope{Access: auth.AccessManage}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return token
	}
	identity, other := token("desktop-owner"), token("other-credential")
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "system", Name: "System", Version: 1,
		ImageDigest: "sha256:" + strings.Repeat("1", 64), Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"root","desktop_profile":"software-vnc-v1"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterRuntime(ctx, route, "test-socket", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('vm','vm','system',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,
			runtime_instance_id,runtime_bundle_id,runtime_instance_object_id,execution_epoch,incarnation_id)
		VALUES ('binding','vm',1,'vm','current','task-ns','digest','serve','bundle','native-vm',4,'vmm-4');
		UPDATE machines SET current_binding_id='binding' WHERE id='vm';
		INSERT INTO services(id,machine_id,name,revision,kind,guest_port,readiness_json,access_json,enabled)
		VALUES ('desktop','vm','Desktop',1,'vnc',5900,'{}','{"private":true}',1)`); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, requestBody, bearer string, status int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(requestBody))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Origin", conf.API.PublicOrigin)
		r.Header.Set("If-Match", `"1"`)
		r.Header.Set("If-Control-Epoch", ctl.ControlEpoch)
		r.Header.Set("Idempotency-Key", "test-desktop")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	create := `{"service_id":"desktop","expected_generation":1,"ttl_seconds":60}`
	const collection = "/v1/machines/vm/desktop-sessions"
	first := do(http.MethodPost, collection, create, identity, http.StatusOK)
	if replay := do(http.MethodPost, collection, create, identity, http.StatusOK); string(first) != string(replay) {
		t.Fatalf("desktop replay changed the receipt: %s %s", first, replay)
	}
	do(http.MethodPost, collection, create, other, http.StatusForbidden)
	var session desktopSession
	if err := json.Unmarshal(first, &session); err != nil || session.ID == "" || session.LeaseID == "" || session.Epoch != 4 || session.State != "active" {
		t.Fatal(session, err)
	}
	path := "/v1/desktop-sessions/" + session.ID
	do(http.MethodGet, path, "", other, http.StatusForbidden)
	do(http.MethodPost, "/v1/services/desktop/tunnel-tickets", "", identity, http.StatusConflict)
	do(http.MethodPost, path+"/tunnel-tickets", "", identity, http.StatusOK)
	if err := db.ExpireDesktopLeases(ctx, session.Expires.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	do(http.MethodPost, path+"/tunnel-tickets", "", identity, http.StatusConflict)
	var current desktopSession
	if err := json.Unmarshal(do(http.MethodGet, path, "", identity, http.StatusOK), &current); err != nil || current.State != "released" || current.Revision != 2 {
		t.Fatal(current, err)
	}
}
