package nodegateway

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/nodes"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

// Exercise the production platformd registration and poll loop, including its
// registry-backed authorization. Only the host budget and artifact facts are
// synthetic; each Node serves its own TLS handshake, journal and gate.
func TestControllerEnrollsAndSamplesTwoLoopbackNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	dir := t.TempDir()
	db, err := registry.Open(ctx, filepath.Join(dir, "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cp, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t)
	authority := nodeproto.Authority{ControllerID: cp.InstallationID, ControlEpoch: cp.ControlEpoch, EnrollmentGeneration: 1}
	cert, pin := ca.leaf(t, nodeproto.ControllerURI(authority), x509.ExtKeyUsageClientAuth)
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, bytes []byte) string {
		t.Helper()
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, bytes, 0600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	caFile := write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
	certFile := write("controller.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	keyFile := write("controller.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	manifest := map[string]any{"schema": "RuntimeBundle/1", "platform": "linux-x86_64", "contracts": map[string]any{"state_reader_id": "reader"}}
	raw, _ := json.Marshal(manifest)
	bundle := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	manifest["bundle_digest"] = bundle
	raw, _ = json.Marshal(manifest)
	manifestPath := write("manifest.json", raw)
	var configNodes []config.Node
	for _, id := range []string{"worker-a", "worker-b"} {
		identity := authority
		identity.WorkerID = id
		route := nodeproto.Route{RuntimeInstanceID: "global-" + id, LocalInstanceID: "same-serve", BundleDigest: bundle}
		j := testJournal(t, filepath.Join(dir, id), identity)
		node := testNode(t, ca, identity, route, fake(id), j, cert, pin, func(s *Server, mux *http.ServeMux) {
			rootRPC(s, mux, nodeproto.SamplePath, func(ctx context.Context, q nodeproto.SampleRequest) (nodeproto.SampleReply, error) {
				return nodeproto.SampleReply{Sample: placement.Sample{WorkerID: id, ControlEpoch: cp.ControlEpoch, EnrollmentGeneration: 1, QueryNonce: q.Nonce,
					Source: placement.SourceWorkerBudget, Sequence: 1, Session: placement.Session{HostBootID: s.bootID, BrokerSessionID: "broker-" + id, NodeSessionID: s.session},
					Budget:   placement.Budget{Capacity: placement.Resources{VCPUs: 4, MemoryMiB: 8192, StorageBytes: 1 << 34, HeavySlots: 1}},
					Pressure: &placement.Pressure{AvailableMemoryMiB: 8192, DiskFreeBytes: 1 << 34},
					Profiles: []placement.RuntimeProfile{{RuntimeInstanceID: route.RuntimeInstanceID, LocalInstanceID: route.LocalInstanceID, BundleID: bundle, Reader: "reader",
						Architecture: "x86_64", Hypervisor: "kvm", RuntimeProcessID: "serve-" + id, Ready: true, Capabilities: []string{rt.SystemBootCapability}}}}}, nil
			})
		})
		configNodes = append(configNodes, config.Node{ID: id, Name: id, Generation: 1, Endpoint: node.server.URL, Pin: node.config.NodeSPKIPin, CA: caFile, Certificate: certFile, Key: keyFile,
			Routes: []config.NodeRoute{{Route: route, Reader: "reader", Manifest: manifestPath}}})
	}
	adapters := make(map[rt.BundleRoute]rt.Adapter)
	workers, err := nodes.Register(ctx, db, configNodes, adapters)
	if err != nil || len(adapters) != 2 {
		t.Fatalf("registration returned %d adapters: %v", len(adapters), err)
	}
	engine := lifecycle.New(db, rt.NewResolver(nil, adapters), config.Defaults())
	done := make(chan struct{})
	go func() { defer close(done); nodes.Run(ctx, workers, engine) }()
	defer func() { cancel(); <-done }()
	for _, n := range configNodes {
		for {
			w, err := db.GetWorker(ctx, n.ID)
			if err != nil {
				t.Fatal(err)
			}
			if w.ObservedStatus == "ready" {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("worker %s ended %s: %v", w.ID, w.ObservedStatus, ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		s, _, err := db.GetWorkerSample(ctx, n.ID)
		if err != nil || !s.Reconciled || s.Profiles[0].RuntimeInstanceID != n.Routes[0].RuntimeInstanceID || s.WorkerID != n.ID {
			t.Fatalf("bad registered sample for %s: %+v %v", n.ID, s, err)
		}
	}
}
