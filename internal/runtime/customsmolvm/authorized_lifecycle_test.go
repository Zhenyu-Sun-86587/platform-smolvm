package customsmolvm

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rt "sandbox-platform/internal/runtime"
)

func TestAuthorizedLifecycleRouteUsesPinnedOptionalProof(t *testing.T) {
	for _, withLifecycle := range []bool{false, true} {
		t.Run(fmt.Sprintf("lifecycle=%t", withLifecycle), func(t *testing.T) {
			root := t.TempDir()
			var artifacts []releaseArtifact
			writeArtifact := func(name string, data []byte) string {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				sha := fmt.Sprintf("%x", sha256.Sum256(data))
				artifacts = append(artifacts, releaseArtifact{Path: name, SHA256: sha, Size: int64(len(data))})
				return "sha256:" + sha
			}
			bundle := "sha256:" + strings.Repeat("a", 64)
			harness := "sha256:" + strings.Repeat("b", 64)
			record := map[string]any{
				"schema": "RuntimeManagedAcceptance/4", "measurement_bundle_digest": bundle,
				"runs": map[string]any{"system-boot": map[string]string{"harness_sha256": harness}},
			}
			if withLifecycle {
				ref := rt.ExecutionRef{InstanceID: "owned-slot", IncarnationID: "owned-incarnation", ExecutionEpoch: 1}
				restarted := rt.ExecutionRef{InstanceID: ref.InstanceID, IncarnationID: "new-incarnation", ExecutionEpoch: 2}
				witness, err := json.Marshal(map[string]any{
					"case": "system-lifecycle", "bundle_digest": bundle,
					"execution": ref, "standby": ref, "resumed": ref,
					"protected_revision": 2, "cleared_revision": 3,
					"wake_operation_id": "owned-wake", "released_lease_id": "owned-lease",
					"cold_started": restarted, "stop_operation_id": "owned-stop", "start_operation_id": "owned-start",
				})
				if err != nil {
					t.Fatal(err)
				}
				log := writeArtifact(lifecycleLog, append([]byte("Authorized KVM "), witness...))
				record["authorized_lifecycle"] = map[string]string{
					"schema": "RuntimeAuthorizedLifecycle/1", "log_sha256": log, "harness_sha256": harness,
				}
			}
			payload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			writeArtifact(managedAcceptance, payload)
			manifest := map[string]any{
				"schema": "RuntimeBundle/1", "platform": "linux-x86_64", "artifacts": artifacts,
				"contracts": map[string]any{"state_reader_id": "test-reader", "managed_authorized_scope": 1},
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			bundle = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			manifest["bundle_digest"] = bundle
			raw, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "runtime-bundle.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			route := Route{InstanceID: "owned-route", Bundle: bundle, Reader: "test-reader", Manifest: path,
				Socket: filepath.Join(root, "serve.sock"), WorkerSocket: filepath.Join(root, "broker.sock")}
			adapter, err := New(route)
			if err != nil {
				t.Fatal(err)
			}
			if got := adapter.LifecycleWorkerSocket(); (got == route.WorkerSocket) != withLifecycle {
				t.Fatalf("unexpected lifecycle socket %q", got)
			}
			if adapter.NativeStopContract() != withLifecycle || adapter.Admit("delete") == nil {
				t.Fatal("authorized lifecycle enabled a historical operation")
			}
			resolver := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route.Key(): adapter})
			if err := resolver.Admit(route.Key(), rt.Standby); (err == nil) != withLifecycle {
				t.Fatalf("unexpected Standby admission: %v", err)
			}
		})
	}
}
