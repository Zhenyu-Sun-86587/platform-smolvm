package smolvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-platform/internal/runtime"
)

func TestCreateResponseLostObservesOriginalName(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	const name = "p1m-original-g1"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == machinesPath:
			var request createBody
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Name != name || request.NetworkBackend != "virtio-net" || request.DiskDurability != "full" {
				t.Errorf("unexpected Create request: %+v, %v", request, err)
			}
			calls.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == machinesPath+"/"+name:
			fmt.Fprintf(w, `{"name":%q,"state":"created","networkBackend":"virtio-net","diskDurability":"full"}`, name)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	client, err := New(socket)
	if err != nil {
		t.Fatal(err)
	}
	const image = "library/alpine@sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f"
	ctx := context.Background()
	_, err = client.Create(ctx, runtime.CreateRequest{Image: image, Name: name})
	if !errors.Is(err, runtime.ErrUncertain) {
		t.Fatalf("lost response returned %v", err)
	}
	got, err := client.Observe(ctx, name)
	if err != nil || got.State != runtime.StateCreated || got.Name != name || calls.Load() != 1 {
		t.Fatalf("observation = %+v, error = %v, mutation calls = %d", got, err, calls.Load())
	}
}

func TestNativeIdentityNeverFilledFromPID(t *testing.T) {
	for _, body := range []string{
		`{"name":"n","state":"running","pid":42}`,
		`{"name":"n","state":"running","runtime":{"pid":42}}`,
		`{"name":"n","state":"running","pid":41,"runtime":{"pid":42,"startTime":567}}`,
	} {
		var machine machineInfo
		if err := json.Unmarshal([]byte(body), &machine); err != nil {
			t.Fatal(err)
		}
		if got := machine.observe(); got.State != runtime.StateUnknown || got.Identity != nil {
			t.Fatalf("ambiguous identity published: %+v", got)
		}
	}
}

type fixtureTransport struct {
	handler http.HandlerFunc
}

func (transport fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	result := httptest.NewRecorder()
	transport.handler.ServeHTTP(result, r)
	return result.Result(), nil
}

func TestOfficialAdapterVersionAndPauseContract(t *testing.T) {
	const name = "legacy-vm"
	const pauseID = "legacy-pause"
	matchingVersion := false
	readyCalls := 0
	client, err := New("/fixture/api.sock")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = fixtureTransport{handler: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			version := "1.37.2"
			if matchingVersion {
				version = "1.37.1"
			}
			fmt.Fprintf(w, "{\"status\":\"healthy\",\"version\":%q}", version)
		case r.Method == http.MethodGet && r.URL.Path == "/readyz":
			readyCalls++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Query().Get("operation_id") == pauseID:
			switch r.URL.Path {
			case machinesPath + "/" + name + "/pause":
				fmt.Fprintf(w, "{\"name\":%q,\"state\":\"paused\"}", name)
			case machinesPath + "/" + name + "/resume":
				fmt.Fprintf(w, "{\"name\":%q,\"state\":\"running\",\"runtime\":{\"pid\":42,\"startTime\":1}}", name)
			default:
				t.Errorf("unexpected lifecycle path %s", r.URL.Path)
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}}
	adapter, err := runtime.NewResolver(client, nil).ForBinding(runtime.BindingRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := adapter.Ready(ctx); !errors.Is(err, runtime.ErrUnsupported) || readyCalls != 0 {
		t.Fatalf("unsupported runtime returned %v after %d readiness calls", err, readyCalls)
	}
	matchingVersion = true
	if err := adapter.Ready(ctx); err != nil || readyCalls != 1 {
		t.Fatalf("pinned runtime returned %v after %d readiness calls", err, readyCalls)
	}
	paused, err := adapter.Pause(ctx, name, pauseID)
	if err != nil || paused.RuntimePauseID != pauseID || paused.Observation.State != runtime.StatePaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	resumed, err := adapter.Resume(ctx, name, paused.RuntimePauseID)
	if err != nil || resumed.RuntimePauseID != pauseID || resumed.Observation.State != runtime.StateRunning || !resumed.Observation.Identity.Complete() {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
}

func TestStartReceiptCarriesWorkloadFailure(t *testing.T) {
	client, err := New("/fixture/api.sock")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = fixtureTransport{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != machinesPath+"/example/start" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "{\"name\":\"example\",\"state\":\"running\",\"runtime\":{\"pid\":42,\"startTime\":1},\"workloadError\":\"entrypoint failed\"}")
	}}
	started, err := client.Start(context.Background(), "example")
	o := started.Observation
	if err != nil || o.State != runtime.StateRunning || !o.Identity.Complete() || o.WorkloadError == nil || *o.WorkloadError != "entrypoint failed" {
		t.Fatalf("start = %+v, %v", o, err)
	}
}

func TestOfficialRuntimeSmoke(t *testing.T) {
	socket, imageDir, image := os.Getenv("SMOLVM_TEST_SOCKET"), os.Getenv("SMOLVM_TEST_IMAGE_DIR"), os.Getenv("SMOLVM_TEST_IMAGE")
	if socket == "" || imageDir == "" || image == "" {
		t.Skip("set the official runtime socket and a local OCI archive for the real VM test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := New(socket, WithLocalImageDir(imageDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("w01-adapter-%d", time.Now().UnixNano())
	created, err := client.Create(ctx, runtime.CreateRequest{Name: name, LocalImage: image, CPUs: 1, MemoryMB: 384, StorageGB: 2, OverlayGB: 2})
	if err != nil || created.State != runtime.StateCreated {
		t.Fatalf("create: %+v, %v", created, err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.Delete(closeCtx, name); err != nil {
			t.Errorf("delete test fixture %s: %v", name, err)
		}
	})
	started, err := client.Start(ctx, name)
	if err != nil || started.Observation.State != runtime.StateRunning || !started.Observation.Identity.Complete() {
		t.Fatalf("start: %+v, %v", started, err)
	}
	t.Logf("start name=%s pid=%s startTime=%s bootId=%s", name, started.Observation.Identity.PID,
		started.Observation.Identity.StartTime, started.Observation.Identity.BootID)
	observed, err := client.Observe(ctx, name)
	if err != nil || !observed.Identity.Equal(started.Observation.Identity) {
		t.Fatalf("observe: %+v, %v", observed, err)
	}
	if result, err := client.Exec(ctx, name, runtime.ExecRequest{Command: []string{"/bin/sh", "-c", "printf adapter-ready"}}); err != nil || result.ExitCode != 0 || string(result.Stdout) != "adapter-ready" {
		t.Fatalf("exec: %+v, %v", result, err)
	}
	if _, err := client.Capacity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Metrics(ctx, name); err != nil {
		t.Fatal(err)
	}
	pauseID := "w01-pause"
	paused, err := client.Pause(ctx, name, pauseID)
	if err != nil || paused.Observation.State != runtime.StatePaused || paused.RuntimePauseID != pauseID {
		t.Fatalf("pause: %+v, %v", paused, err)
	}
	resumed, err := client.Resume(ctx, name, paused.RuntimePauseID)
	if err != nil || resumed.Observation.State != runtime.StateRunning || !resumed.Observation.Identity.Complete() || resumed.RuntimePauseID != pauseID {
		t.Fatalf("resume: %+v, %v", resumed, err)
	}
	t.Logf("resume retained native pause ID %q and reported pid=%s startTime=%s",
		resumed.RuntimePauseID, resumed.Observation.Identity.PID, resumed.Observation.Identity.StartTime)
	if result, err := client.Exec(ctx, name, runtime.ExecRequest{Command: []string{"/bin/sh", "-c", "printf resumed-ready"}}); err != nil || result.ExitCode != 0 || string(result.Stdout) != "resumed-ready" {
		t.Fatalf("exec after resume: %+v, %v", result, err)
	}
	stopped, err := client.Stop(ctx, name)
	if err != nil || stopped.Observation.State != runtime.StateStopped {
		t.Fatalf("stop: %+v, %v", stopped, err)
	}
}
