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
	for _, mode := range []string{"core", "lifecycle", "owner"} {
		t.Run(mode, func(t *testing.T) {
			withLifecycle, withOwner := mode != "core", mode == "owner"
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
			ref := rt.ExecutionRef{InstanceID: "owned-slot", IncarnationID: "owned-incarnation", ExecutionEpoch: 1}
			restarted := rt.ExecutionRef{InstanceID: ref.InstanceID, IncarnationID: "new-incarnation", ExecutionEpoch: 2}
			writeWitness := func(name, path string, fields map[string]any) string {
				fields["case"], fields["bundle_digest"] = name, bundle
				witness, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				return writeArtifact(path, append([]byte("Authorized KVM "), witness...))
			}
			if withLifecycle {
				log := writeWitness("system-lifecycle", lifecycleLog, map[string]any{
					"execution": ref, "standby": ref, "resumed": ref,
					"protected_revision": 2, "cleared_revision": 3,
					"wake_operation_id": "owned-wake", "released_lease_id": "owned-lease",
					"cold_started": restarted, "stop_operation_id": "owned-stop", "start_operation_id": "owned-start",
				})
				record["authorized_lifecycle"] = map[string]string{
					"schema": "RuntimeAuthorizedLifecycle/1", "log_sha256": log, "harness_sha256": harness,
				}
			}
			if withOwner {
				tasks := writeWitness("system-tasks-delete", "metadata/managed/system-tasks-delete.log", map[string]any{
					"original": ref, "execution": restarted, "task_id": "task", "capture_id": "capture", "marker": "task-marker",
					"task_operation_id": "start", "cancel_operation_id": "cancel", "stop_operation_id": "stop", "delete_operation_id": "delete",
				})
				rollback := writeWitness("system-rollback", "metadata/managed/system-rollback.log", map[string]any{
					"original": ref, "restored": restarted, "snapshot_id": "snapshot", "operation_id": "operation",
					"rescue_capture_id": "rescue", "marker": "rollback-marker",
				})
				record["runs"].(map[string]any)["system-restore"] = map[string]string{"harness_sha256": harness}
				record["authorized_owner"] = map[string]any{"schema": "RuntimeAuthorizedOwner/1", "runs": map[string]authorizedRun{
					"system-tasks-delete": {Log: tasks, Harness: harness},
					"system-rollback":     {Log: rollback, Harness: harness},
				}}
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
			if adapter.NativeStopContract() != withLifecycle || adapter.authorized.rollback != withOwner {
				t.Fatal("unexpected authorized power or rollback capability")
			}
			for _, kind := range []string{"task_start", "task_cancel", "delete"} {
				if (adapter.Admit(kind) == nil) != withOwner {
					t.Fatalf("unexpected owner capability for %s", kind)
				}
			}
			resolver := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route.Key(): adapter})
			if err := resolver.Admit(route.Key(), rt.Standby); (err == nil) != withLifecycle {
				t.Fatalf("unexpected Standby admission: %v", err)
			}
		})
	}
}
