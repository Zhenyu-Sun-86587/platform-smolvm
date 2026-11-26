package platformapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

type managedSSHTestRuntime struct {
	rt.Adapter
	rt.NativeGuestControl
}

func TestManagedSSHCertificateAuthorizationAndBinding(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	db, err := registry.Open(ctx, filepath.Join(cfg.StateDir, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	route := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	rtResolver := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route: &managedSSHTestRuntime{}})
	s := New(lifecycle.New(db, rtResolver, cfg), &snapshots.Catalog{DB: db}, nil)
	owner, err := s.Auth.Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range []registry.Template{
		{ID: "system", Name: "Managed System", Version: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64),
			Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"system-base","managed_ssh":true}`)},
		{ID: "legacy", Name: "Legacy", Version: 1, ImageDigest: "sha256:" + strings.Repeat("b", 64)},
	} {
		if err := db.EnsureTemplate(ctx, template); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RegisterRuntime(ctx, route, "isolated-socket", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('parent','parent','system',1,1,'running','{}','{}',0),
		       ('child','child','system',1,1,'stopped','{}','{}',0),
		       ('old','old','legacy',1,1,'running','{}','{}',0),
		       ('deleted','deleted','system',1,1,'stopped','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,
			runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
		VALUES ('parent-b','parent',1,'parent','current','parent','digest','serve','bundle','parent-object','parent-inc',1,'standby'),
		       ('child-b','child',1,'child','current','child','digest','serve','bundle','child-object',NULL,NULL,'stopped');
		UPDATE machines SET current_binding_id=id||'-b' WHERE id IN ('parent','child');
		UPDATE machines SET deleted_at=1 WHERE id='deleted';`); err != nil {
		t.Fatal(err)
	}
	issueToken := func(scope auth.Scope) (auth.Token, string) {
		t.Helper()
		tx, err := db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		token, secret, err := s.Auth.IssueTokenTx(ctx, tx, owner, "test", scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return token, secret
	}
	parentToken, parent := issueToken(auth.Scope{Access: auth.AccessManage, MachineID: "parent"})
	_, global := issueToken(auth.Scope{Access: auth.AccessManage})
	_, read := issueToken(auth.Scope{Access: auth.AccessRead, MachineID: "parent"})
	_, tunnel := issueToken(auth.Scope{Access: auth.AccessTunnel, MachineID: "parent"})
	session, err := s.Auth.Session(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}
	body := func(publicKey string) string {
		t.Helper()
		raw, err := json.Marshal(map[string]string{"public_key": publicKey})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	plain := string(ssh.MarshalAuthorizedKey(key))
	post := func(machine, bearer, input string, cookie, withCSRF bool, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/machines/"+machine+"/ssh-certificates", strings.NewReader(input))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie {
			r.AddCookie(&http.Cookie{Name: s.sessionCookie().Name, Value: session})
		}
		if withCSRF {
			r.Header.Set("Origin", cfg.API.PublicOrigin)
			r.Header.Set("X-CSRF-Token", csrf(session))
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: got %d, want %d: %s", machine, w.Code, status, w.Body.String())
		}
		return w
	}
	post("parent", "", body(plain), false, false, http.StatusUnauthorized)
	post("parent", "", body(plain), true, false, http.StatusForbidden)
	post("parent", read, body(plain), false, false, http.StatusForbidden)
	post("parent", tunnel, body(plain), false, false, http.StatusForbidden)
	post("child", parent, body(plain), false, false, http.StatusForbidden)
	post("old", global, body(plain), false, false, http.StatusConflict)
	post("deleted", global, body(plain), false, false, http.StatusNotFound)
	for _, bad := range []string{
		"malformed", "cert-authority " + plain, plain + plain, strings.Repeat(" ", managedSSHCertificateRequestLimit) + plain,
	} {
		post("parent", parent, body(bad), false, false, http.StatusBadRequest)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "secrets", "ssh-user-ca")); !os.IsNotExist(err) {
		t.Fatalf("rejected requests created a signing authority: %v", err)
	}

	readCertificate := func(w *httptest.ResponseRecorder, machine string) *ssh.Certificate {
		t.Helper()
		var response struct {
			Certificate string    `json:"certificate"`
			ExpiresAt   time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(response.Certificate))
		cert, ok := pub.(*ssh.Certificate)
		if err != nil || !ok || len(options) != 0 || len(rest) != 0 {
			t.Fatalf("invalid OpenSSH certificate response: %v", err)
		}
		if cert.CertType != ssh.UserCert || cert.KeyId != machine || !bytes.Equal(cert.Key.Marshal(), key.Marshal()) ||
			len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "root" ||
			cert.ValidBefore-cert.ValidAfter != 120 || uint64(response.ExpiresAt.Unix()) != cert.ValidBefore {
			t.Fatalf("unexpected certificate scope: %+v", cert)
		}
		if len(cert.CriticalOptions) != 0 || len(cert.Extensions) != 2 {
			t.Fatalf("unexpected certificate permissions: %+v", cert.Permissions)
		}
		if _, ok := cert.Extensions["permit-pty"]; !ok {
			t.Fatal("PTY permission missing")
		}
		if _, ok := cert.Extensions["permit-port-forwarding"]; !ok {
			t.Fatal("port forwarding permission missing")
		}
		checker := ssh.CertChecker{Clock: func() time.Time { return time.Unix(int64(cert.ValidAfter), 0) }}
		if checker.CheckCert("root", cert) != nil || checker.CheckCert("other", cert) == nil {
			t.Fatal("certificate not limited to root")
		}
		checker.Clock = func() time.Time { return response.ExpiresAt }
		if checker.CheckCert("root", cert) == nil {
			t.Fatal("expired certificate accepted")
		}
		return cert
	}
	cert := readCertificate(post("parent", parent, body(plain), false, false, http.StatusOK), "parent")
	child := readCertificate(post("child", global, body(plain), false, false, http.StatusOK), "child")
	renewed := readCertificate(post("parent", "", body(plain), true, true, http.StatusOK), "parent")
	if bytes.Equal(cert.SignatureKey.Marshal(), child.SignatureKey.Marshal()) ||
		!bytes.Equal(cert.SignatureKey.Marshal(), renewed.SignatureKey.Marshal()) {
		t.Fatal("signing authority was not persisted per machine")
	}
	post("parent", parent, body(string(ssh.MarshalAuthorizedKey(cert))), false, false, http.StatusBadRequest)
	if _, err := db.DB().ExecContext(ctx, "UPDATE credentials SET revoked_at=1 WHERE id=?", parentToken.ID); err != nil {
		t.Fatal(err)
	}
	post("parent", parent, body(plain), false, false, http.StatusUnauthorized)
}
