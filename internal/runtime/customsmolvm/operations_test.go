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

func TestQueryMatchesReceiptToOriginalOperationAndBundle(t *testing.T) {
	bundle := "sha256:" + strings.Repeat("a", 64)
	h := Handshake{
		Schema: "runtime-ext/v1", RouteID: "deep", BundleDigest: bundle, StateReaderID: "reader",
		Protocols:        map[string]int{"RuntimeBundle": 1, "WorkerResource": 1, "StatePoint": 1, "CaptureControl": 1, "GuestIdentity": 1, "NetworkAttachment": 1, "BlockAttachment": 1},
		RequiredFeatures: []string{}, Capabilities: map[string]rt.Capability{},
	}
	sock := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := json.NewEncoder(w)
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		if r.URL.Path == capabilitiesPath {
			_ = reply.Encode(h)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, extensionPath+"/operations/")
		receipt := OperationReceipt{OperationID: id, Kind: "catalog", RequestDigest: bundle, RouteID: "deep", InstanceID: "target", BundleDigest: bundle, Phase: "NEW", Decision: "NONE", Revision: 1}
		switch id {
		case "mismatched-id":
			receipt.OperationID = "original"
		case "legacy":
			receipt.RouteID = "official"
		case "other-bundle":
			receipt.BundleDigest = "sha256:" + strings.Repeat("b", 64)
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			_ = reply.Encode(map[string]string{"code": "E_NOT_FOUND", "error": "absent"})
			return
		}
		_ = reply.Encode(receipt)
	})}
	go server.Serve(l)
	t.Cleanup(func() { _ = server.Close() })
	for _, id := range []string{"original", "mismatched-id", "legacy", "other-bundle", "missing", "bad/id"} {
		got, err := QueryOperation(context.Background(), sock, bundle, "reader", id)
		switch id {
		case "original":
			if err != nil || got.OperationID != id {
				t.Fatal(got, err)
			}
		case "missing":
			var native *rt.Error
			if !errors.As(err, &native) || native.NativeCode != "E_NOT_FOUND" || !errors.Is(err, rt.ErrNotFound) {
				t.Fatalf("missing receipt: %v", err)
			}
		case "bad/id":
			if !errors.Is(err, rt.ErrInvalid) {
				t.Fatal(err)
			}
		default:
			if !errors.Is(err, rt.ErrUnsupported) {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}
}
