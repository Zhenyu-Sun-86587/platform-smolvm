package customsmolvm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	rt "sandbox-platform/internal/runtime"
)

func TestAdapterRequiresExactInstanceOnTheObservationConnection(t *testing.T) {
	ctx := context.Background()
	bundle := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, advertised string
		closeHandshake   bool
		want             error
	}{
		{name: "match", advertised: "serve-a"},
		{name: "wrong instance", advertised: "serve-b", want: rt.ErrUnsupported},
		{name: "old diagnostic handshake", want: rt.ErrUnsupported},
		{name: "lost connection", advertised: "serve-a", closeHandshake: true, want: rt.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "s")
			listener, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			var observations atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				switch r.URL.Path {
				case capabilitiesPath:
					if tc.closeHandshake {
						w.Header().Set("Connection", "close")
					}
					_ = json.NewEncoder(w).Encode(Handshake{
						Schema: "runtime-ext/v1", RouteID: "deep", RuntimeInstanceID: tc.advertised,
						BundleDigest: bundle, StateReaderID: "reader", RequiredFeatures: []string{},
						Protocols:    map[string]int{"RuntimeBundle": 1, "WorkerResource": 1, "StatePoint": 1, "CaptureControl": 1, "GuestIdentity": 1, "NetworkAttachment": 1, "BlockAttachment": 1},
						Capabilities: map[string]rt.Capability{"storage.block_attach": {Status: "unknown"}},
					})
				case "/api/v1/machines/example":
					observations.Add(1)
					_, _ = w.Write([]byte(`{"name":"example","state":"running","runtime":{"pid":42,"startTime":3},"workloadError":"entrypoint failed"}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})}
			go server.Serve(listener)
			t.Cleanup(func() { _ = server.Close() })
			adapter, err := New(Route{InstanceID: "serve-a", Bundle: bundle, Reader: "reader", Socket: sock})
			if err != nil {
				t.Fatal(err)
			}
			observation, err := adapter.Observe(ctx, "example")
			if !errors.Is(err, tc.want) {
				t.Fatalf("observe = %+v, %v; want %v", observation, err, tc.want)
			}
			if tc.want != nil {
				if observations.Load() != 0 {
					t.Fatal("incompatible Serve received an observation request")
				}
				return
			}
			if observation.State != rt.StateRunning || observation.WorkloadError == nil ||
				*observation.WorkloadError != "entrypoint failed" || observation.Capabilities["storage.block_attach"].Supported {
				t.Fatalf("invalid observation: %+v", observation)
			}
			if _, err := adapter.Create(ctx, rt.CreateRequest{Name: "example"}); !errors.Is(err, rt.ErrUnsupported) {
				t.Fatalf("create bypassed native admission: %v", err)
			}
		})
	}
}
