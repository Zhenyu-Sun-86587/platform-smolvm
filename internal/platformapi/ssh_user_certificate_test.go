package platformapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	"sandbox-platform/internal/runtime/customsmolvm"
	"sandbox-platform/internal/runtime/workerclient"
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
		VALUES ('m_parent','parent','system',1,1,'running','{}','{}',0),
		       ('m_child','child','system',1,1,'stopped','{}','{}',0),
		       ('m_old','old','legacy',1,1,'running','{}','{}',0),
		       ('m_deleted','deleted','system',1,1,'stopped','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,
			runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
		VALUES ('m_parent-b','m_parent',1,'parent','current','parent','digest','serve','bundle','parent-object','parent-inc',1,'standby'),
		       ('m_child-b','m_child',1,'child','current','child','digest','serve','bundle','child-object',NULL,NULL,'stopped');
		UPDATE machines SET current_binding_id=id||'-b' WHERE id IN ('m_parent','m_child');
		UPDATE machines SET deleted_at=1 WHERE id='m_deleted';`); err != nil {
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
	parentToken, parent := issueToken(auth.Scope{Access: auth.AccessManage, MachineID: "m_parent"})
	_, global := issueToken(auth.Scope{Access: auth.AccessManage})
	_, read := issueToken(auth.Scope{Access: auth.AccessRead, MachineID: "m_parent"})
	_, tunnel := issueToken(auth.Scope{Access: auth.AccessTunnel, MachineID: "m_parent"})
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
	post("m_parent", "", body(plain), false, false, http.StatusUnauthorized)
	post("m_parent", "", body(plain), true, false, http.StatusForbidden)
	post("m_parent", read, body(plain), false, false, http.StatusForbidden)
	post("m_parent", tunnel, body(plain), false, false, http.StatusForbidden)
	post("m_child", parent, body(plain), false, false, http.StatusForbidden)
	post("m_old", global, body(plain), false, false, http.StatusConflict)
	post("m_deleted", global, body(plain), false, false, http.StatusNotFound)
	for _, bad := range []string{
		"malformed", "cert-authority " + plain, plain + plain, strings.Repeat(" ", managedSSHCertificateRequestLimit) + plain,
	} {
		post("m_parent", parent, body(bad), false, false, http.StatusBadRequest)
	}
	for _, response := range []*httptest.ResponseRecorder{
		post("m_parent", parent, body(plain), false, false, http.StatusConflict),
		post("m_child", global, body(plain), false, false, http.StatusConflict),
		post("m_parent", "", body(plain), true, true, http.StatusConflict),
	} {
		if !strings.Contains(response.Body.String(), `"code":"SSH_ROUTER_UNVERIFIED"`) {
			t.Fatal(response.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "secrets", "ssh-user-ca")); !os.IsNotExist(err) {
		t.Fatalf("rejected requests created a signing authority: %v", err)
	}

	readCertificate := func(machine string) *ssh.Certificate {
		t.Helper()
		source := netip.MustParsePrefix("10.40.0.1/32")
		signed, expires, err := s.SignManagedSSHCertificate(machine, key, source)
		if err != nil {
			t.Fatal(err)
		}
		pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(signed))
		cert, ok := pub.(*ssh.Certificate)
		if err != nil || !ok || len(options) != 0 || len(rest) != 0 {
			t.Fatalf("invalid OpenSSH certificate response: %v", err)
		}
		if cert.CertType != ssh.UserCert || cert.KeyId != machine || !bytes.Equal(cert.Key.Marshal(), key.Marshal()) ||
			len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "root" ||
			cert.ValidBefore-cert.ValidAfter != 120 || uint64(expires.Unix()) != cert.ValidBefore {
			t.Fatalf("unexpected certificate scope: %+v", cert)
		}
		if len(cert.CriticalOptions) != 1 || cert.CriticalOptions["source-address"] != source.String() || len(cert.Extensions) != 2 {
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
		checker.Clock = func() time.Time { return expires }
		if checker.CheckCert("root", cert) == nil {
			t.Fatal("expired certificate accepted")
		}
		return cert
	}
	cert := readCertificate("m_parent")
	child := readCertificate("m_child")
	renewed := readCertificate("m_parent")
	if bytes.Equal(cert.SignatureKey.Marshal(), child.SignatureKey.Marshal()) ||
		!bytes.Equal(cert.SignatureKey.Marshal(), renewed.SignatureKey.Marshal()) {
		t.Fatal("signing authority was not persisted per machine")
	}
	post("m_parent", parent, body(string(ssh.MarshalAuthorizedKey(cert))), false, false, http.StatusBadRequest)
	if _, err := db.DB().ExecContext(ctx, "UPDATE credentials SET revoked_at=1 WHERE id=?", parentToken.ID); err != nil {
		t.Fatal(err)
	}
	post("m_parent", parent, body(plain), false, false, http.StatusUnauthorized)
}

func TestManagedSSHRouterPrefixUsesExactWorkerNetwork(t *testing.T) {
	binding := registry.Binding{RuntimeInstanceID: "serve", RuntimeBundleID: "bundle"}
	const socket = "/worker"
	target := customsmolvm.PrivateNetwork{ID: "network", RouteDomainID: "domain", CIDR: "10.40.0.0/24"}
	other := customsmolvm.PrivateNetwork{ID: "another", RouteDomainID: "another-domain", CIDR: "10.50.0.0/24"}
	route := customsmolvm.Route{InstanceID: "serve", Bundle: "bundle", WorkerSocket: socket,
		DefaultNetworkID: other.ID, PrivateNetworks: []customsmolvm.PrivateNetwork{other, target}}
	launch := rt.LaunchNetwork{NetworkID: target.ID, Mode: rt.FixedInternalNetwork, RouteDomainID: target.RouteDomainID, CIDR: target.CIDR}
	guest := netip.MustParseAddr("10.40.0.20")
	routes := []customsmolvm.Route{route}
	if matched := sshRouterPrefix(routes, binding, socket, launch, target.ID, guest); matched.String() != "10.40.0.1/32" {
		t.Fatalf("router source = %v", matched)
	}
	if sshRouterPrefix(routes, binding, "/another-worker", launch, target.ID, guest).IsValid() ||
		sshRouterPrefix(routes, binding, socket, launch, other.ID, guest).IsValid() ||
		sshRouterPrefix(routes, binding, socket, launch, target.ID, netip.MustParseAddr("10.40.0.255")).IsValid() ||
		sshRouterPrefix([]customsmolvm.Route{route, route}, binding, socket, launch, target.ID, guest).IsValid() {
		t.Fatal("accepted unknown or ambiguous Worker topology")
	}
	for _, cidr := range []string{"10.40.0.0/25", "10.40.0.1/24", "10.0.0.0/7"} {
		changed := target
		changed.CIDR = cidr
		route.PrivateNetworks = []customsmolvm.PrivateNetwork{changed}
		if sshRouterPrefix([]customsmolvm.Route{route}, binding, socket, launch, target.ID, guest).IsValid() {
			t.Fatalf("accepted changed router CIDR %s", cidr)
		}
	}
	route.PrivateNetworks = []customsmolvm.PrivateNetwork{target, target}
	if sshRouterPrefix([]customsmolvm.Route{route}, binding, socket, launch, target.ID, guest).IsValid() {
		t.Fatal("accepted ambiguous network ID")
	}
}

func TestManagedSSHRouterLeaseDuringStandby(t *testing.T) {
	routed := netip.MustParseAddr("10.40.0.20")
	guest := netip.MustParseAddr("192.168.100.2")
	lease := workerclient.NetworkLease{State: "attached", Gate: "closed"}
	lease.Attachment.Address = routed
	lease.Attachment.GuestAddress = routed
	lease.Attachment.Mode = rt.NativeNetwork
	if !sshRouterLeaseVerified(lease, rt.NativeNetwork) {
		t.Fatal("standby's attached closed gate lost its router evidence")
	}
	lease.Attachment.GuestAddress = guest
	lease.Attachment.Mode = rt.FixedInternalNetwork
	if !sshRouterLeaseVerified(lease, rt.FixedInternalNetwork) || sshRouterLeaseVerified(lease, rt.NativeNetwork) {
		t.Fatal("fixed internal lease accepted under the wrong profile")
	}
	for _, transient := range []struct{ state, gate string }{
		{"quarantined", "closed"}, {"offered", "closed"}, {"retired", "open"},
		{"attached", "opening"}, {"attached", "unknown"},
	} {
		lease.State, lease.Gate = transient.state, transient.gate
		if sshRouterLeaseVerified(lease, rt.FixedInternalNetwork) {
			t.Fatalf("accepted unverified lease %s/%s", lease.State, lease.Gate)
		}
	}
	if !sshNetworkAddressesMatch(rt.NativeNetwork, routed, routed) ||
		!sshNetworkAddressesMatch(rt.FixedInternalNetwork, routed, guest) ||
		sshNetworkAddressesMatch(rt.FixedInternalNetwork, routed, routed) ||
		sshNetworkAddressesMatch(rt.NativeNetwork, routed, guest) ||
		sshNetworkAddressesMatch(rt.FixedInternalNetwork, routed, netip.Addr{}) {
		t.Fatal("invalid saved attachment address evidence")
	}
}
