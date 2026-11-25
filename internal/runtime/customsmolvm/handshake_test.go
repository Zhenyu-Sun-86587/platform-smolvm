package customsmolvm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	rt "sandbox-platform/internal/runtime"
)

func TestHandshakeRequiresExactBundleAndKnownProtocol(t *testing.T) {
	bundle := "sha256:" + strings.Repeat("a", 64)
	const reader = "state-reader-1"
	for _, test := range []struct {
		name      string
		change    func(*Handshake)
		wantError bool
	}{
		{"locked", func(*Handshake) {}, false},
		{"wrong reader", func(h *Handshake) { h.StateReaderID = "reader-2" }, true},
		{"wrong bundle", func(h *Handshake) { h.BundleDigest = "sha256:" + strings.Repeat("b", 64) }, true},
		{"unknown required feature", func(h *Handshake) { h.RequiredFeatures = []string{"reader/v2"} }, true},
		{"unsupported capture major", func(h *Handshake) { h.Protocols["CaptureControl"] = 2 }, true},
		{"contradictory capability", func(h *Handshake) {
			h.Capabilities["state.hibernate"] = rt.Capability{Status: "unknown", Supported: true}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := Handshake{Schema: "runtime-ext/v1", RouteID: "deep", BundleDigest: bundle, StateReaderID: reader,
				Protocols:        map[string]int{"RuntimeBundle": 1, "WorkerResource": 1, "StatePoint": 1, "CaptureControl": 1, "GuestIdentity": 1, "NetworkAttachment": 1, "BlockAttachment": 1},
				RequiredFeatures: []string{}, Capabilities: map[string]rt.Capability{"state.hibernate": {Status: "unknown", Supported: false}}}
			test.change(&h)
			socket := filepath.Join(t.TempDir(), "s")
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			s := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != capabilitiesPath || r.Method != http.MethodGet {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(h)
			})}
			go s.Serve(l)
			t.Cleanup(func() { _ = s.Close() })
			got, err := Probe(context.Background(), socket, bundle, reader)
			if test.wantError {
				if !errors.Is(err, rt.ErrUnsupported) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil || got.BundleDigest != bundle {
				t.Fatal(got, err)
			}
		})
	}
}
