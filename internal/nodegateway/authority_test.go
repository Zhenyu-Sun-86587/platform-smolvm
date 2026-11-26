package nodegateway

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	wc "sandbox-platform/internal/runtime/workerclient"
)

func TestOfflineEnrollmentRotationKeepsGateAndRecoverRebindDrains(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "journal")
	initial := testAuthority
	j := testJournal(t, dir, initial)
	gate, err := j.setGate(ctx, nodeproto.SetGate{Mode: nodeproto.GateCordoned, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	next := initial
	next.EnrollmentGeneration++
	if err := RebindJournal(dir, initial, next); err == nil {
		t.Fatal("a running Node did not hold exclusive authority")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	skip := next
	skip.EnrollmentGeneration++
	if err := RebindJournal(dir, initial, skip); err == nil {
		t.Fatal("ordinary certificate rotation skipped a generation")
	}
	wrong := next
	wrong.WorkerID = "worker-b"
	if err := RebindJournal(dir, initial, wrong); err == nil {
		t.Fatal("another Worker could take the same journal")
	}
	if err := RebindJournal(dir, initial, next); err != nil {
		t.Fatal(err)
	}
	if old, err := OpenJournal(dir, initial); err == nil {
		old.Close()
		t.Fatal("journal still allows the prior enrollment")
	}
	j = testJournal(t, dir, next)
	actual, err := readGate(ctx, j.db)
	if err != nil || actual != gate {
		t.Fatalf("ordinary rotation changed the gate: %+v / %v", actual, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	recovering := next
	recovering.ControllerID = "recovered-installation"
	if err := RebindJournal(dir, next, recovering); err == nil {
		t.Fatal("controller installation changed without a new enrollment")
	}
	recovering.EnrollmentGeneration += 2
	if err := RebindJournal(dir, next, recovering); err != nil {
		t.Fatal(err)
	}
	j = testJournal(t, dir, recovering)
	actual, err = readGate(ctx, j.db)
	if err != nil || actual.Mode != nodeproto.GateDraining || actual.Revision != gate.Revision+1 {
		t.Fatalf("recovered installation did not enter Drain: %+v / %v", actual, err)
	}
}

func TestLongNodeStreamsRenewAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	backupFile, feedBackup, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer backupFile.Close()
	defer feedBackup.Close()
	controlReceived := make(chan struct{})
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"),
		testJournal(t, filepath.Join(t.TempDir(), "journal"), testAuthority), controller, pin,
		func(s *Server, mux *http.ServeMux) {
			stream(s, mux, nodeproto.ServiceStreamPath,
				func(in nodeproto.ServiceStream) rt.ExecutionRef { return in.Expected },
				func(ctx context.Context, _ Route, _ nodeproto.ServiceStream) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", tcp.Addr().String())
				})
			backupRPC(s, mux, make(chan struct{}, 1), nodeproto.BackupMemberPath,
				func(context.Context, nodeproto.Route, Route, nodeproto.BackupMember) (backupSource, error) {
					return backupSource{file: backupFile, size: 1}, nil
				})
			mux.HandleFunc("POST "+nodeproto.WorkerPath+"resize", func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(controlReceived)
				<-r.Context().Done()
			})
		})
	var revoked atomic.Bool
	config := node.config
	config.Authorize = func(context.Context) error {
		if revoked.Load() {
			return &nodeproto.Failure{Code: nodeproto.CodeAuthority}
		}
		return nil
	}
	ad := testClient(t, config).Adapter(testRoute)
	conn, err := ad.DialService(ctx, testRef, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	local, err := tcp.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	read, _, err := ad.BackupMember(ctx, backupID(testRoute), "blocks/held")
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	controlResult := make(chan error, 1)
	go func() {
		_, err := ad.ResizeResources(ctx, "held-resize", workerExecution(testRef), wc.ResourceChange{Kind: "cpu_grow", CPUs: 2})
		controlResult <- err
	}()
	select {
	case <-controlReceived:
	case <-ctx.Done():
		t.Fatal("control request did not reach the Worker")
	}
	revoked.Store(true)
	if err := local.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = local.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("existing framed stream survived authority revocation: %v", err)
	}
	if _, err = read.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("existing backup stream survived authority revocation: %v", err)
	}
	if err := <-controlResult; !errors.Is(err, rt.ErrUncertain) || ctx.Err() != nil {
		t.Fatalf("revoked in-flight mutation did not become Unknown before the request deadline: %v", err)
	}
	if _, err := ad.ComputeResources(ctx, workerExecution(testRef)); nodeproto.FailureCode(err) != nodeproto.CodeAuthority {
		t.Fatalf("cached adapter still sent control requests: %v", err)
	}
}

func TestLostWorkerRPCReplyDistinguishesWritesAndReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	var writes atomic.Int32
	lost := func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		writes.Add(1)
		transport, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = transport.Close()
	}
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"),
		testJournal(t, filepath.Join(t.TempDir(), "journal"), testAuthority), controller, pin,
		func(_ *Server, mux *http.ServeMux) {
			for _, name := range []string{"policy/ApplyLifecyclePolicy", "policy/GetLifecycleStatus", "resize", "packet-rule"} {
				mux.HandleFunc("POST "+nodeproto.WorkerPath+name, lost)
			}
		})
	a := node.client.Adapter(testRoute)
	ref := workerExecution(testRef)
	on := true
	for name, err := range map[string]error{
		"policy mutation": a.Lifecycle(ctx, "ApplyLifecyclePolicy", "op-1", ref, struct{}{}, nil),
		"resize mutation": func() error {
			_, err := a.ResizeResources(ctx, "op-2", ref, wc.ResourceChange{Kind: "cpu_grow", CPUs: 2})
			return err
		}(),
		"packet mutation": func() error {
			_, err := a.PacketWakeRule(ctx, ref, wc.PacketWakeRule{Enabled: &on}, "op-3", testDigest)
			return err
		}(),
	} {
		if !errors.Is(err, rt.ErrUncertain) || !wc.RetryDelivery(err) {
			t.Errorf("%s lost reply did not remain Unknown: %v", name, err)
		}
	}
	if err := a.Lifecycle(ctx, "GetLifecycleStatus", "", ref, struct{}{}, nil); !errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain) {
		t.Fatalf("lost read was an uncertain write: %v", err)
	}
	if _, err := a.PacketWakeRule(ctx, ref, wc.PacketWakeRule{}, "", ""); !errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain) {
		t.Fatalf("read-only packet lookup was an uncertain write: %v", err)
	}
	if writes.Load() != 5 {
		t.Fatalf("one or more Worker requests were replayed: %d", writes.Load())
	}

	denied := node.config
	denied.Authorize = func(context.Context) error { return &nodeproto.Failure{Code: nodeproto.CodeAuthority} }
	_, err := testClient(t, denied).Adapter(testRoute).ResizeResources(ctx, "never-sent", ref, wc.ResourceChange{Kind: "cpu_grow", CPUs: 2})
	if nodeproto.FailureCode(err) != nodeproto.CodeAuthority || errors.Is(err, rt.ErrUncertain) || writes.Load() != 5 {
		t.Fatalf("a pre-send denial was treated as an uncertain write: %v", err)
	}
}
