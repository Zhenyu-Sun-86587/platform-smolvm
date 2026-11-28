package nodegateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

func TestJournalRestartAndLossFenceWorkerSessions(t *testing.T) {
	ctx := t.Context()
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
	if err := db.RegisterWorker(ctx, registry.Worker{ID: authority.WorkerID, Name: "Worker test"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: authority.WorkerID, Generation: 1,
		CertificateFingerprint: "test-pin", Authority: control.ControlEpoch}); err != nil {
		t.Fatal(err)
	}
	route := rt.BundleRoute{WorkerID: authority.WorkerID, InstanceID: testRoute.RuntimeInstanceID,
		LocalInstanceID: testRoute.LocalInstanceID, BundleID: testRoute.BundleDigest}
	if err := db.RegisterRuntime(ctx, route, route.LocalInstanceID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	ca := newTestCA(t)
	controller, controllerPin := ca.leaf(t, nodeproto.ControllerURI(authority), x509.ExtKeyUsageClientAuth)
	node, nodePin := ca.leaf(t, nodeproto.WorkerURI(authority), x509.ExtKeyUsageServerAuth)
	directory := filepath.Join(t.TempDir(), "original")
	newGateway := func(dir string) (*Server, *Journal, *fakeRuntime) {
		t.Helper()
		j, err := OpenJournal(dir, authority)
		if err != nil {
			t.Fatal(err)
		}
		backend := fake("worker")
		s, err := New(authority, map[nodeproto.Route]Route{testRoute: {Adapter: backend}}, j)
		if err != nil {
			j.Close()
			t.Fatal(err)
		}
		return s, j, backend
	}
	first, firstJournal, firstBackend := newGateway(directory)
	var handler atomic.Value
	handler.Store(first.Handler(controllerPin))
	tlsConfig, err := first.TLSConfig(node, ca.roots, controllerPin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	client := testClient(t, nodeclient.Config{Endpoint: server.URL, Authority: authority, NodeSPKIPin: nodePin,
		TLS: &tls.Config{Certificates: []tls.Certificate{controller}, RootCAs: ca.roots},
		SelectNode: func(ctx context.Context, candidate nodeproto.NodeIdentity) (nodeproto.NodeIdentity, error) {
			id, err := db.WorkerNodeIdentity(ctx, authority, candidate)
			if errors.Is(err, registry.ErrNotFound) {
				return id, &nodeproto.Failure{Code: nodeproto.CodeAuthority}
			}
			return id, err
		}})
	adapter := client.Adapter(testRoute)
	approve := func(hello nodeproto.Handshake, nonce string) {
		t.Helper()
		sample := placement.Sample{WorkerID: authority.WorkerID, EnrollmentGeneration: 1, ControlEpoch: control.ControlEpoch,
			Source: placement.SourceWorkerBudget, QueryNonce: nonce, Sequence: 1, Reconciled: true,
			Session:  placement.Session{HostBootID: hello.HostBootID, BrokerSessionID: "broker", NodeSessionID: hello.NodeSession},
			Budget:   placement.Budget{Capacity: placement.Resources{VCPUs: 4, MemoryMiB: 8192, StorageBytes: 1 << 34, HeavySlots: 1}},
			Pressure: &placement.Pressure{AvailableMemoryMiB: 8192, DiskFreeBytes: 1 << 34},
			Profiles: []placement.RuntimeProfile{{RuntimeInstanceID: route.InstanceID, LocalInstanceID: route.LocalInstanceID,
				BundleID: route.BundleID, Architecture: "x86_64", Hypervisor: "kvm", RuntimeProcessID: "process", Reader: "reader", Ready: true}}}
		started := time.Now()
		if err := db.RecordWorkerSample(ctx, sample, placement.Query{Nonce: nonce, StartedAt: started, CompletedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	assertDenied := func(err error) {
		t.Helper()
		var failure *nodeproto.Failure
		if !errors.As(err, &failure) || failure.Code != nodeproto.CodeAuthority {
			t.Fatalf("expected the Node identity fence, got %v", err)
		}
	}

	original, err := client.Handshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PinWorkerJournal(ctx, authority, original.Identity(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.SubmitNative(ctx, request("before-reconciliation")); err == nil {
		t.Fatal("ordinary mutation accepted before the first reconciled sample")
	} else {
		assertDenied(err)
	}
	approve(original, "sample-one")
	if _, err := adapter.SubmitNative(ctx, request("original-operation")); err != nil || firstBackend.sends != 1 {
		t.Fatal(firstBackend.sends, err)
	}
	if err := firstJournal.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, secondJournal, secondBackend := newGateway(directory)
	defer secondJournal.Close()
	// Serve and resourced retain the same operation across the Node restart.
	secondBackend.receipts["original-operation"] = firstBackend.receipts["original-operation"]
	handler.Store(restarted.Handler(controllerPin))
	second, err := client.Handshake(ctx)
	if err != nil || second.JournalID != original.JournalID || second.NodeSession == original.NodeSession {
		t.Fatal(second, err)
	}
	if err := db.PinWorkerJournal(ctx, authority, second.Identity(), ""); err != nil {
		t.Fatal(err)
	}
	probe := nodeclient.WithReconciliationIdentity(ctx, second.Identity())
	gate, err := client.Gate(probe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.SetGate(probe, nodeproto.SetGate{Mode: nodeproto.GateActive, ExpectedRevision: gate.Revision}); err == nil {
		t.Fatal("unapproved session accepted a normal gate write")
	} else {
		assertDenied(err)
	}
	if gate, err = client.SetGate(probe, nodeproto.SetGate{Mode: nodeproto.GateCordoned, ExpectedRevision: gate.Revision, RestrictOnly: true}); err != nil || gate.Mode != nodeproto.GateCordoned {
		t.Fatal(gate, err)
	}
	if got, err := adapter.ReconcileKnown(probe, nodeproto.ReconcileRequest{KnownIDsOnly: true, Operations: []string{"original-operation"}}); err != nil ||
		len(got.Operations) != 1 || got.Operations[0].Receipt == nil {
		t.Fatalf("cannot reconcile the original operation after restart: %+v %v", got, err)
	}
	_, err = adapter.SubmitNative(probe, request("unapproved-session-operation"))
	assertDenied(err)
	_, err = adapter.DialService(ctx, testRef, 80)
	assertDenied(err)
	_, _, err = adapter.BackupMember(ctx, backupID(testRoute), "blocks/example")
	assertDenied(err)
	if secondBackend.sends != 0 {
		t.Fatalf("unapproved session sent %d native requests", secondBackend.sends)
	}
	approve(second, "sample-two")
	if _, err := adapter.SubmitNative(ctx, request("approved-session-operation")); err != nil || secondBackend.sends != 1 {
		t.Fatal(secondBackend.sends, err)
	}

	replaced, newJournal, newBackend := newGateway(filepath.Join(t.TempDir(), "replaced"))
	defer newJournal.Close()
	handler.Store(replaced.Handler(controllerPin))
	lost, err := client.Handshake(ctx)
	if err != nil || lost.JournalID == second.JournalID {
		t.Fatal(lost, err)
	}
	if err = db.PinWorkerJournal(ctx, authority, lost.Identity(), ""); !errors.Is(err, registry.ErrWorkerIdentityConflict) {
		t.Fatalf("same-certificate replacement was not quarantined: %v", err)
	}
	_, err = adapter.SubmitNative(ctx, request("approved-session-operation"))
	assertDenied(err)
	_, err = adapter.OpenScopedGuest(ctx, workerclient.Execution{InstanceID: testRef.InstanceID, IncarnationID: testRef.IncarnationID, Epoch: testRef.ExecutionEpoch})
	assertDenied(err)
	if newBackend.sends != 0 {
		t.Fatalf("replacement journal sent %d native requests", newBackend.sends)
	}
}
