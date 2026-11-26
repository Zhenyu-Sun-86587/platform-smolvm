package nodegateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/platformapi"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"sandbox-platform/internal/snapshots"
)

type guestRelayTestRuntime struct {
	*fakeRuntime
	ref rt.ExecutionRef
}

func (a guestRelayTestRuntime) DiscoverExecution(_ context.Context, name string) (rt.ExecutionRef, error) {
	if name != "guest-slot" {
		return rt.ExecutionRef{}, rt.ErrNotFound
	}
	return a.ref, nil
}

type guestRelayOnlyAdapter struct {
	rt.Adapter
	rt.GuestRelayBridge
}

func TestRemoteGuestRelayAttestsAndCancelsCredentialProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	control, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority
	authority.ControlEpoch, authority.ControllerID = control.ControlEpoch, control.InstallationID

	ca := newTestCA(t)
	controller, controllerPin := ca.leaf(t, nodeproto.ControllerURI(authority), x509.ExtKeyUsageClientAuth)
	node, nodePin := ca.leaf(t, nodeproto.WorkerURI(authority), x509.ExtKeyUsageServerAuth)
	shortDir, err := os.MkdirTemp("", "relay-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shortDir)
	directory := filepath.Join(shortDir, "guest")
	ref := rt.ExecutionRef{InstanceID: "guest-id", IncarnationID: "vmm-" + strings.Repeat("a", 32), ExecutionEpoch: 4}
	backend := guestRelayTestRuntime{fakeRuntime: fake("guest-worker"), ref: ref}
	var transportAttested atomic.Bool
	checked := make(chan bool, 32)
	gateway, err := New(authority, map[nodeproto.Route]Route{testRoute: {
		Adapter: backend, GuestRelayDir: directory, Worker: workerclient.Client{Socket: "/unused/worker.sock", Bundle: testRoute.BundleDigest},
		verifyGuestTransport: func(_ context.Context, execution workerclient.Execution, conn *net.UnixConn) error {
			accepted := transportAttested.Load() && execution == workerExecution(ref) && conn != nil
			select {
			case checked <- accepted:
			default:
			}
			if !accepted {
				return errors.New("fixture denied unverified socket")
			}
			return nil
		},
	}}, testJournal(t, filepath.Join(t.TempDir(), "journal"), authority))
	if err != nil {
		t.Fatal(err)
	}
	go gateway.RunGuestRelays(ctx)
	tlsConfig, err := gateway.TLSConfig(node, ca.roots, controllerPin)
	if err != nil {
		t.Fatal(err)
	}
	httpsNode := httptest.NewUnstartedServer(gateway.Handler(controllerPin))
	httpsNode.TLS = tlsConfig
	httpsNode.StartTLS()
	defer httpsNode.Close()
	var epochValid atomic.Bool
	epochValid.Store(true)
	client := testClient(t, nodeclient.Config{Endpoint: httpsNode.URL, Authority: authority, NodeSPKIPin: nodePin,
		TLS: &tls.Config{Certificates: []tls.Certificate{controller}, RootCAs: ca.roots},
		Authorize: func(context.Context) error {
			if !epochValid.Load() {
				return &nodeproto.Failure{Code: nodeproto.CodeAuthority}
			}
			return nil
		}})
	if _, err := client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	adapter := client.Adapter(testRoute)
	if !adapter.GuestRelaySupported() {
		t.Fatal("Guest relay capability was not advertised")
	}
	if _, err := adapter.AcceptGuestRelay(ctx, rt.GuestRelayScope{BindingID: "bind", Name: "different-vm", Execution: ref}); err == nil {
		t.Fatal("Node allowed an execution through the wrong runtime slot")
	}

	route := rt.BundleRoute{WorkerID: authority.WorkerID, InstanceID: testRoute.RuntimeInstanceID,
		LocalInstanceID: testRoute.LocalInstanceID, BundleID: testRoute.BundleDigest}
	if err := db.RegisterWorker(ctx, registry.Worker{ID: route.WorkerID, Name: "Node"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterRuntime(ctx, route, route.LocalInstanceID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "tpl", Name: "base", Version: 1, ImageDigest: "image"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running,worker_id)
		VALUES('machine','guest','tpl',1,1,'running','{}','{}',0,?);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,worker_id,runtime_instance_id,
		  runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
		VALUES('bind','machine',1,'guest-slot','current','ns','spec',?,?,?,?,?,?,'running');
		UPDATE machines SET current_binding_id='bind' WHERE id='machine';`,
		route.WorkerID, route.WorkerID, route.InstanceID, route.BundleID, ref.InstanceID, ref.IncarnationID, ref.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	authn := auth.New(db)
	owner, err := authn.Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authn.CreateGuestGrant(ctx, owner, auth.GuestGrantSpec{
		SourceMachineID: "machine", Action: auth.GuestProxy, ProxyID: "llm", MaximumUses: 3, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := authn.IssueGuestCredential(ctx, owner, auth.AttestedGuestContext{MachineID: "machine", BindingID: "bind",
		RuntimeInstanceID: route.InstanceID, RuntimeBundleID: route.BundleID, RuntimeObjectID: ref.InstanceID,
		IncarnationID: ref.IncarnationID, ExecutionEpoch: ref.ExecutionEpoch}, grant, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	const providerSecret = "fixture-provider-key"
	upstreamStarted, upstreamCancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+providerSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/invoke":
			_, _ = io.WriteString(w, "provider echoed "+providerSecret)
		case "/slow":
			close(upstreamStarted)
			<-r.Context().Done()
			close(upstreamCancelled)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	providerKey, upstreamCA := filepath.Join(t.TempDir(), "token"), filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(providerKey, []byte(providerSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upstreamCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.Defaults()
	c.CredentialProxies = []config.CredentialProxy{{ID: "llm", Upstream: upstream.URL, CredentialFile: providerKey, CAFile: upstreamCA, AllowPrivate: true,
		Routes:            []config.CredentialProxyRoute{{Method: "GET", Path: "/invoke"}, {Method: "GET", Path: "/slow"}},
		RequestsPerMinute: 5, RequestBytes: 1024, ResponseBytes: 1024, TimeoutSeconds: 6}}
	e := lifecycle.New(db, rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route: guestRelayOnlyAdapter{Adapter: adapter, GuestRelayBridge: adapter}}), c)
	cp := platformapi.New(e, &snapshots.Catalog{DB: db}, http.NotFoundHandler())
	if err := cp.StartGuestRelays(ctx); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", c.API.PublicOrigin+"/v1/control", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Attested-Worker", authority.WorkerID)
	public := httptest.NewRecorder()
	cp.ServeHTTP(public, req)
	if public.Code != http.StatusUnauthorized {
		t.Fatalf("public API treated a Guest bearer as authenticated: %d", public.Code)
	}

	socket := filepath.Join(directory, ref.IncarnationID+".sock")
	unixHTTP := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			for {
				conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
				if err == nil {
					return conn, nil
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
			}
		},
	}}
	call := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://guest"+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return unixHTTP.Do(req)
	}
	if reply, err := call("/v1/control"); err == nil {
		reply.Body.Close()
		t.Fatal("unverified Unix connection reached the controller")
	}
	select {
	case accepted := <-checked:
		if accepted {
			t.Fatal("first Unix connection appeared attested")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Node did not inspect the accepted socket")
	}
	transportAttested.Store(true)

	reply, err := call("/v1/credential-proxies/other/invoke")
	if err != nil {
		t.Fatal(err)
	}
	reply.Body.Close()
	if reply.StatusCode != http.StatusForbidden {
		t.Fatalf("credential accepted for wrong audience: %d", reply.StatusCode)
	}
	reply, err = call("/v1/credential-proxies/llm/invoke")
	if err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(reply.Body)
	reply.Body.Close()
	if err != nil || reply.StatusCode != http.StatusOK || string(response) != "provider echoed [redacted]" {
		t.Fatalf("verified credential proxy response: %d %q %v", reply.StatusCode, response, err)
	}

	stopped := make(chan struct{})
	go func() {
		if reply, err := call("/v1/credential-proxies/llm/slow"); err == nil {
			_ = reply.Body.Close()
		}
		close(stopped)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Guest credential proxy call did not reach HTTPS upstream")
	}
	transportAttested.Store(false)
	select {
	case <-upstreamCancelled:
	case <-time.After(4 * time.Second):
		t.Fatal("revoked Unix transport did not cancel the credential request")
	}
	<-stopped
}
