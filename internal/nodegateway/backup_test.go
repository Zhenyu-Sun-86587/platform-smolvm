package nodegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/runtime/workerclient"
)

func backupID(scope nodeproto.Route) string {
	return "test-backup-" + nodeproto.BackupSuffix(scope.RuntimeInstanceID, scope.BundleDigest)
}

func TestRemoteBackupDigestAndIncompleteMembers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	dir := t.TempDir()
	data := bytes.Repeat([]byte("verified worker bytes\n"), 64<<10)
	name := filepath.Join(dir, "member")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"), testJournal(t, filepath.Join(dir, "journal"), testAuthority), controller, pin,
		func(s *Server, mux *http.ServeMux) {
			backupRPC(s, mux, make(chan struct{}, 2), nodeproto.BackupMemberPath,
				func(_ context.Context, _ nodeproto.Route, _ Route, in nodeproto.BackupMember) (backupSource, error) {
					f, err := os.Open(name)
					size := int64(len(data))
					if in.Member == "blocks/truncated" {
						size += 4096
					}
					return backupSource{file: f, size: size}, err
				})
			mux.HandleFunc("POST "+nodeproto.BackupManifestPath, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set(nodeproto.BackupSizeHeader, strconv.Itoa(len(data)))
				w.Header().Set(nodeproto.BackupManifestHeader, fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
				w.Header().Set("Trailer", nodeproto.BackupDigestTrailer)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = w.Write(data)
				w.Header().Set(nodeproto.BackupDigestTrailer, "sha256:"+strings.Repeat("1", 64))
			})
		})
	adapter := node.client.Adapter(testRoute)
	read, size, err := adapter.BackupMember(ctx, backupID(testRoute), "blocks/full")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(read)
	read.Close()
	if err != nil || size != int64(len(data)) || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("complete backup member: size=%d read=%d err=%v", size, len(got), err)
	}
	read, size, err = adapter.BackupMember(ctx, backupID(testRoute), "blocks/truncated")
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(read)
	read.Close()
	if err == nil || size <= int64(len(got)) || !bytes.Equal(got, data) {
		t.Fatalf("truncated stream was acknowledged: size=%d read=%d err=%v", size, len(got), err)
	}
	badDigest, _, err := adapter.BackupManifest(ctx, workerclient.BackupSelection{ID: backupID(testRoute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, badDigest); err == nil {
		t.Fatal("client accepted a mismatched content digest trailer")
	}
	badDigest.Close()
}

func TestRemoteBackupLimitsLeaveControlAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	dir := filepath.Join(t.TempDir(), "journal")
	j := testJournal(t, dir, testAuthority)
	const blocked = 2
	readers := make(map[string]*os.File, blocked)
	writers := make([]*os.File, blocked)
	for i := range blocked {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		readers[fmt.Sprintf("blocks/%d", i)], writers[i] = r, w
	}
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"), j, controller, pin,
		func(s *Server, mux *http.ServeMux) {
			backupRPC(s, mux, make(chan struct{}, blocked), nodeproto.BackupMemberPath,
				func(_ context.Context, _ nodeproto.Route, _ Route, in nodeproto.BackupMember) (backupSource, error) {
					r := readers[in.Member]
					if r == nil {
						t.Error("backup opened new member while stream slots were reserved")
						return backupSource{}, fmt.Errorf("unexpected member")
					}
					return backupSource{file: r, size: 1}, nil
				})
		})
	a := node.client.Adapter(testRoute)
	var streams []io.ReadCloser
	for i := range blocked {
		body, _, err := a.BackupMember(ctx, backupID(testRoute), fmt.Sprintf("blocks/%d", i))
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		streams = append(streams, body)
	}
	status, result := backupHTTPStatus(t, ctx, node, nodeproto.BackupMember{ID: backupID(testRoute), Member: "blocks/exceeds-limit"})
	if status != http.StatusTooManyRequests || result.Code != nodeproto.CodeBusy {
		t.Fatalf("stream limit did not reject a third backup: status=%d failure=%+v", status, result)
	}
	probe, release := context.WithTimeout(ctx, time.Second)
	defer release()
	if _, err := node.client.Handshake(probe); err != nil {
		t.Fatalf("reserved backup slots starved control: %v", err)
	}
	for i, stream := range streams {
		if _, err := writers[i].Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		writers[i].Close()
		if body, err := io.ReadAll(stream); err != nil || string(body) != "x" {
			t.Fatalf("blocked member %d did not complete: %q / %v", i, body, err)
		}
	}
}

func TestRemoteBackupRejectsPathsAndRouteAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"),
		testJournal(t, filepath.Join(t.TempDir(), "journal"), testAuthority), controller, pin, nil)
	other := testRoute
	other.RuntimeInstanceID = "another-runtime"
	for _, bad := range []nodeproto.BackupMember{
		{ID: backupID(testRoute), Member: "../../etc/passwd"},
		{ID: backupID(testRoute), Member: "blocks/../etc/passwd"},
		{ID: backupID(testRoute), Member: "blocks/\\root"},
		{ID: backupID(testRoute), Member: "/statepoints/capture"},
		{ID: backupID(other), Member: "blocks/valid"},
	} {
		status, result := backupHTTPStatus(t, ctx, node, bad)
		if status != http.StatusBadRequest || result.Code != nodeproto.CodeInvalid {
			t.Fatalf("unsafe member or wrong route ID reached Worker: %+v / %d / %+v", bad, status, result)
		}
	}
}

func backupHTTPStatus(t *testing.T, ctx context.Context, node pair, input nodeproto.BackupMember) (int, nodeproto.Failure) {
	t.Helper()
	raw, err := json.Marshal(nodeproto.Envelope[nodeproto.BackupMember]{
		Protocol: nodeproto.Version, Authority: testAuthority, Route: testRoute, Request: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, node.server.URL+nodeproto.BackupMemberPath, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: node.config.TLS.Clone()}
	defer transport.CloseIdleConnections()
	r, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var failure nodeproto.Failure
	if err := json.NewDecoder(r.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, failure
}
