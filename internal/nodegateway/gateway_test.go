package nodegateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

var (
	testAuthority = nodeproto.Authority{WorkerID: "worker-a", EnrollmentGeneration: 1, ControllerID: "platformd", ControlEpoch: "epoch-1"}
	testDigest    = "sha256:" + strings.Repeat("a", 64)
	testRoute     = nodeproto.Route{RuntimeInstanceID: "runtime-a", LocalInstanceID: "identical-serve", BundleDigest: testDigest}
	testRef       = rt.ExecutionRef{InstanceID: "same-vm-id", IncarnationID: "same-incarnation", ExecutionEpoch: 11}
)

type fakeRuntime struct {
	rt.UnsupportedOperations
	name        string
	mu          sync.Mutex
	sends       int
	queries     int
	receipts    map[string]rt.OperationReceipt
	submitReady chan struct{}
	release     chan struct{}
}

func (f *fakeRuntime) SubmitNative(ctx context.Context, request rt.NativeSubmission) (rt.OperationReceipt, error) {
	r := testReceipt(request)
	f.mu.Lock()
	f.sends++
	f.receipts[request.OperationID] = r
	f.mu.Unlock()
	if f.submitReady != nil {
		close(f.submitReady)
		select {
		case <-f.release:
		case <-ctx.Done():
			return rt.OperationReceipt{}, ctx.Err()
		}
	}
	return r, nil
}

func testReceipt(request rt.NativeSubmission) rt.OperationReceipt {
	return rt.OperationReceipt{OperationID: request.OperationID, Kind: request.Kind, RequestDigest: request.RequestDigest,
		BundleDigest: testDigest, RouteID: "deep", ExpectedExecutionEpoch: request.Expected.ExecutionEpoch,
		ExpectedIncarnationID: &request.Expected.IncarnationID, InstanceID: request.Expected.InstanceID,
		Phase: "NEW", Decision: "NONE", Revision: 1}
}

func (f *fakeRuntime) QueryOperation(_ context.Context, operation string) (rt.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	r, ok := f.receipts[operation]
	if !ok {
		return rt.OperationReceipt{}, &rt.Error{Code: "NOT_FOUND", NativeCode: "E_NOT_FOUND"}
	}
	return r, nil
}

func (f *fakeRuntime) DiscoverExecution(context.Context, string) (rt.ExecutionRef, error) {
	return testRef, nil
}
func (f *fakeRuntime) NativeInventory(context.Context, rt.ExecutionRef) (rt.NativeInventory, error) {
	return rt.NativeInventory{}, rt.ErrUnsupported
}
func (f *fakeRuntime) DiscoverLaunchBinding(context.Context, string) (rt.LaunchBinding, error) {
	return rt.LaunchBinding{InstanceID: testRef.InstanceID, IncarnationID: &testRef.IncarnationID, ExecutionEpoch: testRef.ExecutionEpoch}, nil
}
func (f *fakeRuntime) Health(context.Context) (rt.Health, error) { return rt.Health{Status: "ok"}, nil }
func (f *fakeRuntime) Ready(context.Context) error               { return nil }
func (f *fakeRuntime) Observe(context.Context, string) (rt.Observation, error) {
	return rt.Observation{Name: "same-vm", Image: f.name, State: rt.StateRunning}, nil
}
func (f *fakeRuntime) GuestPing(context.Context, rt.ExecutionRef) error { return nil }
func (f *fakeRuntime) GuestExec(_ context.Context, ref rt.ExecutionRef, _ rt.ExecRequest) (rt.ExecResult, error) {
	if ref != testRef {
		return rt.ExecResult{}, rt.ErrConflict
	}
	return rt.ExecResult{Stdout: []byte(f.name)}, nil
}
func (f *fakeRuntime) GuestUpload(context.Context, rt.ExecutionRef, string, io.Reader, rt.FileOptions) error {
	return rt.ErrUnsupported
}

func fake(name string) *fakeRuntime {
	return &fakeRuntime{name: name, receipts: make(map[string]rt.OperationReceipt)}
}

type testCA struct {
	cert  *x509.Certificate
	key   ed25519.PrivateKey
	roots *x509.CertPool
}

func issue(t *testing.T, template, parent *x509.Certificate, public ed25519.PublicKey, signing ed25519.PrivateKey) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signing)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	cert, err := x509.ParseCertificate(issue(t, ca, ca, public, key))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return testCA{cert, key, roots}
}

func (c testCA) leaf(t *testing.T, identity string, usage x509.ExtKeyUsage) (tls.Certificate, string) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: c.cert.NotBefore, NotAfter: c.cert.NotAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature, URIs: []*url.URL{uri},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der := issue(t, leaf, c.cert, public, c.key)
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		fmt.Sprintf("sha256:%x", sha256.Sum256(leaf.RawSubjectPublicKeyInfo))
}

func testJournal(t *testing.T, path string, authority nodeproto.Authority) *Journal {
	t.Helper()
	j, err := OpenJournal(path, authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

type pair struct {
	server *httptest.Server
	client *nodeclient.Client
	config nodeclient.Config
}

func testNode(t *testing.T, ca testCA, authority nodeproto.Authority, route nodeproto.Route, backend rt.Adapter, journal *Journal,
	controller tls.Certificate, controllerPin string, extend func(*Server, *http.ServeMux)) pair {
	t.Helper()
	gateway, err := New(authority, map[nodeproto.Route]Route{route: {Adapter: backend}}, journal)
	if err != nil {
		t.Fatal(err)
	}
	node, nodePin := ca.leaf(t, nodeproto.WorkerURI(authority), x509.ExtKeyUsageServerAuth)
	tlsConfig, err := gateway.TLSConfig(node, ca.roots, controllerPin)
	if err != nil {
		t.Fatal(err)
	}
	handler := gateway.Handler(controllerPin)
	if extend != nil {
		mux := http.NewServeMux()
		extend(gateway, mux)
		mux.Handle("/", handler)
		handler = mux
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	config := nodeclient.Config{Endpoint: server.URL, Authority: authority, NodeSPKIPin: nodePin,
		TLS: &tls.Config{Certificates: []tls.Certificate{controller}, RootCAs: ca.roots}}
	return pair{server: server, client: testClient(t, config), config: config}
}

func testClient(t *testing.T, c nodeclient.Config) *nodeclient.Client {
	t.Helper()
	client, err := nodeclient.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func request(id string) rt.NativeSubmission {
	return rt.NativeSubmission{OperationID: id, Kind: rt.Wake, RequestDigest: testDigest, Expected: testRef}
}

func TestLostResponseQueriesOnlyOriginalWorker(t *testing.T) {
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	workerA, workerB := fake("worker-a"), fake("worker-b")
	workerA.submitReady, workerA.release = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(workerA.release) })
	defer release()
	a := testNode(t, ca, testAuthority, testRoute, workerA, testJournal(t, filepath.Join(t.TempDir(), "a"), testAuthority), controller, pin, nil)
	authorityB, routeB := testAuthority, testRoute
	authorityB.WorkerID, routeB.RuntimeInstanceID = "worker-b", "runtime-b"
	b := testNode(t, ca, authorityB, routeB, workerB, testJournal(t, filepath.Join(t.TempDir(), "b"), authorityB), controller, pin, nil)

	adapter := a.client.Adapter(testRoute)
	wake := request("lost-reply")
	submit, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	result := make(chan error, 1)
	go func() { _, err := adapter.SubmitNative(submit, wake); result <- err }()
	select {
	case <-workerA.submitReady:
	case <-time.After(5 * time.Second):
		t.Fatal("Worker did not receive the native submission")
	}
	disconnect()
	if err := <-result; !errors.Is(err, rt.ErrUncertain) {
		t.Fatalf("lost reply should be uncertain, got %v", err)
	}
	observed, err := adapter.QueryOperation(context.Background(), wake.OperationID)
	if err != nil || observed.OperationID != wake.OperationID {
		t.Fatalf("query did not recover original Worker receipt: %+v / %v", observed, err)
	}
	for _, other := range []nodeproto.Route{testRoute, routeB} {
		if _, err := b.client.Adapter(other).QueryOperation(context.Background(), wake.OperationID); err == nil {
			t.Fatal("query found original operation on another Worker")
		}
	}
	release()
	if _, err := adapter.SubmitNative(context.Background(), wake); err != nil {
		t.Fatalf("duplicate lost request did not return original receipt: %v", err)
	}
	wake.Parameters.Mode = "changed-body-with-same-native-digest"
	if _, err := adapter.SubmitNative(context.Background(), wake); !errors.Is(err, rt.ErrConflict) {
		t.Fatalf("same ID and changed transport digest should conflict: %v", err)
	}
	workerA.mu.Lock()
	defer workerA.mu.Unlock()
	if workerA.sends != 1 || workerB.sends != 0 {
		t.Fatalf("native sends on A/B = %d/%d", workerA.sends, workerB.sends)
	}
}

func TestCrashWindowRebindAndKnownOnly(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "node")
	j := testJournal(t, dir, testAuthority)
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	backend := fake("worker-a")
	first := testNode(t, ca, testAuthority, testRoute, backend, j, controller, pin, nil)
	original, err := first.client.Handshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	crashed := request("crash-window")
	scope := nativeCommand(crashed.Expected, crashed.Kind, crashed.RequestDigest)
	scope.Route = testRoute
	raw, err := json.Marshal(crashed)
	if err != nil {
		t.Fatal(err)
	}
	if _, send, err := j.begin(ctx, crashed.OperationID, submitStep, scope, fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), false, true); err != nil || !send {
		t.Fatalf("could not set crash fence: %v / %v", send, err)
	}
	if _, err := backend.SubmitNative(ctx, crashed); err != nil {
		t.Fatal(err)
	}
	first.server.Close()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j = testJournal(t, dir, testAuthority)
	next := testNode(t, ca, testAuthority, testRoute, backend, j, controller, pin, nil)
	restarted, err := next.client.Handshake(ctx)
	if err != nil || restarted.HostBootID != original.HostBootID || restarted.NodeSession == original.NodeSession {
		t.Fatalf("Node restart should keep boot ID and change only session: %+v / %v", restarted, err)
	}
	if _, err := next.client.Adapter(testRoute).SubmitNative(ctx, crashed); err != nil || backend.sends != 1 {
		t.Fatalf("duplicate after restart re-sent native request: %d / %v", backend.sends, err)
	}
	state, err := next.client.Adapter(testRoute).ReconcileKnown(ctx, nodeproto.ReconcileRequest{Bindings: []string{"same-vm"}})
	if err != nil || state.Coverage != nodeproto.KnownOnly || len(state.Operations) != 1 || state.Operations[0].Receipt == nil ||
		len(state.Bindings) != 1 || state.Bindings[0].Binding == nil {
		t.Fatalf("unexpected known-only reconciliation: %+v / %v", state, err)
	}

	next.server.Close()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	newAuthority := testAuthority
	newAuthority.ControlEpoch = "recovered-epoch"
	newAuthority.EnrollmentGeneration++
	if err := RebindJournal(dir, testAuthority, newAuthority); err != nil {
		t.Fatal(err)
	}
	if old, err := OpenJournal(dir, testAuthority); err == nil {
		old.Close()
		t.Fatal("stale authority was able to open re-bound Node journal")
	}
	j = testJournal(t, dir, newAuthority)
	rebound := testNode(t, ca, newAuthority, testRoute, backend, j, controller, pin, nil)
	stale := rebound.config
	stale.Authority.ControlEpoch = testAuthority.ControlEpoch
	old := testClient(t, stale)
	if _, err := old.Adapter(testRoute).SubmitNative(ctx, request("stale-controller")); nodeproto.FailureCode(err) != nodeproto.CodeAuthority {
		t.Fatalf("old epoch could submit after rebind: %v", err)
	}
	gate, err := rebound.client.Gate(ctx)
	if err != nil || gate.Mode != nodeproto.GateDraining {
		t.Fatalf("new epoch should start draining: %+v / %v", gate, err)
	}
	if _, err := rebound.client.Adapter(testRoute).SubmitNative(ctx, request("new-in-drain")); nodeproto.FailureCode(err) != nodeproto.CodeGateClosed {
		t.Fatalf("new start admitted in Drain: %v", err)
	}
	if _, err := rebound.client.Adapter(testRoute).QueryOperation(ctx, crashed.OperationID); err != nil {
		t.Fatalf("recovery authority lost original operation: %v", err)
	}
	if _, err := rebound.client.SetGate(ctx, nodeproto.SetGate{Mode: nodeproto.GateActive, ExpectedRevision: gate.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebound.client.SetGate(ctx, nodeproto.SetGate{Mode: nodeproto.GateCordoned, ExpectedRevision: gate.Revision}); !errors.Is(err, rt.ErrConflict) {
		t.Fatalf("gate did not require CAS: %v", err)
	}
}

func TestIdentityScopeAndTypedRequests(t *testing.T) {
	ctx := context.Background()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	dir := filepath.Join(t.TempDir(), "node")
	j := testJournal(t, dir, testAuthority)
	a := testNode(t, ca, testAuthority, testRoute, fake("worker-a"), j, controller, pin, nil)
	if parallel, err := OpenJournal(dir, testAuthority); err == nil {
		parallel.Close()
		t.Fatal("two Node processes can open one enrollment")
	}
	for _, mutate := range []func(*nodeclient.Config){
		func(c *nodeclient.Config) { c.Authority.WorkerID = "wrong-worker" },
		func(c *nodeclient.Config) { c.Authority.EnrollmentGeneration++ },
		func(c *nodeclient.Config) { c.NodeSPKIPin = "sha256:" + strings.Repeat("0", 64) },
		func(c *nodeclient.Config) {
			impostor, _ := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
			c.TLS = c.TLS.Clone()
			c.TLS.Certificates = []tls.Certificate{impostor}
		},
	} {
		c := a.config
		mutate(&c)
		if _, err := testClient(t, c).Handshake(ctx); err == nil {
			t.Fatal("wrong enrollment identity or unpinned certificate accepted")
		}
	}
	different := testRoute
	different.LocalInstanceID = "other-serve"
	if _, err := a.client.Adapter(different).Observe(ctx, "same-vm"); nodeproto.FailureCode(err) != nodeproto.CodeRoute {
		t.Fatalf("route mismatch accepted: %v", err)
	}
	rawTLS := a.config.TLS.Clone()
	rawTLS.MinVersion = tls.VersionTLS13
	pinnedTransport := &http.Transport{TLSClientConfig: rawTLS}
	defer pinnedTransport.CloseIdleConnections()
	rawClient := &http.Client{Transport: pinnedTransport}
	invalid, err := json.Marshal(nodeproto.Envelope[map[string]string]{
		Protocol: nodeproto.Version, Authority: testAuthority, Route: testRoute,
		Request: map[string]string{"name": "same-vm", "socket": "/run/other-service.sock", "method": "ArbitraryMethod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rawClient.Post(a.server.URL+nodeproto.ObservePath, "application/json", bytes.NewReader(invalid))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("untyped local path and method accepted: %d", res.StatusCode)
	}

	authorityB, routeB := testAuthority, testRoute
	authorityB.WorkerID, routeB.RuntimeInstanceID = "worker-b", "runtime-b"
	b := testNode(t, ca, authorityB, routeB, fake("worker-b"),
		testJournal(t, filepath.Join(t.TempDir(), "node"), authorityB), controller, pin, nil)
	for client, want := range map[*nodeclient.Adapter]string{a.client.Adapter(testRoute): "worker-a", b.client.Adapter(routeB): "worker-b"} {
		res, err := client.GuestExec(ctx, testRef, rt.ExecRequest{Command: []string{"hostname"}})
		if err != nil || string(res.Stdout) != want {
			t.Fatalf("shared local name/identity resolved to the wrong Worker: %+v / %v", res, err)
		}
	}
}

func TestJournalLeavesUncertainSendFencedAndTerminalHistoryDoesNotFillReconcile(t *testing.T) {
	ctx := context.Background()
	j := testJournal(t, filepath.Join(t.TempDir(), "node"), testAuthority)
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	backend := fake("worker-a")
	node := testNode(t, ca, testAuthority, testRoute, backend, j, controller, pin, nil)
	for i := range nodeproto.ReconcileLimit + 1 {
		wake := request(fmt.Sprintf("operation-%03d", i))
		scope := nativeCommand(wake.Expected, wake.Kind, wake.RequestDigest)
		scope.Route = testRoute
		if _, _, err := j.begin(ctx, wake.OperationID, submitStep, scope, testDigest, false, false); err != nil {
			t.Fatal(err)
		}
		receipt := testReceipt(wake)
		receipt.Phase = "COMPLETED"
		if err := j.remember(ctx, wake.OperationID, submitStep, receipt); err != nil {
			t.Fatal(err)
		}
	}
	wake := request("unseen-after-fence")
	scope := nativeCommand(wake.Expected, wake.Kind, wake.RequestDigest)
	scope.Route = testRoute
	raw, err := json.Marshal(wake)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.begin(ctx, wake.OperationID, submitStep, scope, fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), false, false); err != nil {
		t.Fatal(err)
	}
	a := node.client.Adapter(testRoute)
	if _, err := a.SubmitNative(ctx, wake); !errors.Is(err, rt.ErrUncertain) || backend.sends != 0 {
		t.Fatalf("unobserved native result allowed a second send: %d / %v", backend.sends, err)
	}
	result, err := a.ReconcileKnown(ctx, nodeproto.ReconcileRequest{})
	if err != nil || result.JournalLimited || len(result.Operations) != 1 || result.Operations[0].Issue != nodeproto.CodeUnknown {
		t.Fatalf("journal could not distinguish terminal history and unresolved send: %+v / %v", result, err)
	}
}

func TestStreamHalfCloseAndControlUnderBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	backend := fake("worker-a")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pending, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	node := testNode(t, ca, testAuthority, testRoute, backend,
		testJournal(t, filepath.Join(t.TempDir(), "node"), testAuthority), controller, pin,
		func(s *Server, mux *http.ServeMux) {
			stream(s, mux, nodeproto.ServiceStreamPath,
				func(in nodeproto.ServiceStream) rt.ExecutionRef { return in.Expected },
				func(ctx context.Context, _ Route, in nodeproto.ServiceStream) (net.Conn, error) {
					dialer := net.Dialer{}
					if in.Port == 2 {
						return dialer.DialContext(ctx, "tcp", pending.Addr().String())
					}
					return dialer.DialContext(ctx, "tcp", listener.Addr().String())
				})
		})
	a := node.client.Adapter(testRoute)
	bulk := make([]byte, 4<<20)
	if _, err := rand.Read(bulk); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(bulk)
	workerDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
			digest := sha256.New()
			var count int64
			count, err = io.Copy(digest, conn)
			if err == nil && count != int64(len(bulk)) {
				err = fmt.Errorf("received %d bytes", count)
			}
			if err == nil {
				_, err = conn.Write(digest.Sum(nil))
			}
		}
		workerDone <- err
	}()
	conn, err := a.DialService(ctx, testRef, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for range nodeproto.StreamPerExecution - 1 {
		stalled, err := a.DialService(ctx, testRef, 2)
		if err != nil {
			t.Fatal(err)
		}
		defer stalled.Close()
		accepted, err := pending.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer accepted.Close()
	}
	if stalled, err := a.DialService(ctx, testRef, 2); nodeproto.FailureCode(err) != nodeproto.CodeBusy {
		if stalled != nil {
			stalled.Close()
		}
		t.Fatalf("execution exceeded its stream limit: %v", err)
	}
	if _, err := node.client.Handshake(ctx); err != nil {
		t.Fatalf("streams blocked the control request: %v", err)
	}
	if _, err := conn.Write(bulk); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(conn, 33))
	if err != nil || !bytes.Equal(got, wantDigest[:]) {
		t.Fatalf("half-close lost the response or corrupted stream: %x / %v", got, err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
}

func TestJournalRequiresPrivateStateDirectory(t *testing.T) {
	d := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(d, 0755); err != nil {
		t.Fatal(err)
	}
	if j, err := OpenJournal(d, testAuthority); err == nil {
		j.Close()
		t.Fatal("Node opened publicly shared state directory")
	}
}

func TestRestoreVerifiesNativeTargetReceipt(t *testing.T) {
	expected := rt.LaunchBinding{InstanceID: "pinned-slot", ExecutionEpoch: 3}
	c := bindingCommand(expected, rt.VMRestore, testDigest)
	c.Route = testRoute
	r := rt.OperationReceipt{OperationID: "op", Kind: "restore_target", InstanceID: expected.InstanceID, ExpectedExecutionEpoch: 3,
		RequestDigest: testDigest, BundleDigest: testRoute.BundleDigest, RouteID: "deep", Revision: 1}
	if !c.verifies("op", r) {
		t.Fatal("original native restore target was not recognized")
	}
	r.Kind = rt.Launch
	if c.verifies("op", r) {
		t.Fatal("cold launch impersonated saved restore target")
	}
}
