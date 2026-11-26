package snapshots

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	rt "sandbox-platform/internal/runtime"
)

func TestCaptureConfigurationAndScopeProjection(t *testing.T) {
	const capture = "capture-a"
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const representation = "representation-a"
	incarnation := "execution-a"
	request := rt.StatePointSubmission{OperationID: "op-a", Expected: rt.LaunchBinding{IncarnationID: &incarnation, ExecutionEpoch: 7},
		Parameters: rt.StatePointParameters{CaptureID: capture}}
	point := rt.StatePointProgress{CaptureID: capture, RepresentationID: representation, ManifestDigest: digest}
	worker := json.RawMessage(`{
		"operation_id":"op-a","phase":"durable","manifest_digest":"` + digest + `",
		"point":{"schema":"StatePoint/1","capture_id":"capture-a","representation_id":"representation-a",
		"incarnation_id":"execution-a","execution_epoch":7,"regions":[{"gpa":8192,"length":4096},{"gpa":0,"length":8192}],
		"devices":[{"device_id":"root","virtio_type":2,"block":{"capacity_bytes":1024,"read_only":false,
		"managed_attachment":{"device_id":"root","role":"root","volume_id":"root-v","generation_id":"root-base",
		"writable_head_id":"root-seal","execution_epoch":7,"capacity_bytes":1024}}},
		{"device_id":"data","virtio_type":2,"block":{"capacity_bytes":512,"read_only":true,
		"managed_attachment":{"device_id":"data","role":"data","volume_id":"data-v","generation_id":"data-seal",
		"execution_epoch":7,"capacity_bytes":512}}}],"disk_generations":["root-seal","data-seal"]}}`)
	config, err := CaptureSpec(json.RawMessage(`{"resources":{"memory_mib":512,"cpu_count":1},"boot_mode":"system",
		"network":{"mode":"native","network_id":"private-a","credential":"secret"},"grant":"secret","idle_policy":{"enabled":true}}`), "template-a", 4, capture)
	if err != nil {
		t.Fatal(err)
	}
	config.ManifestDigest = digest
	manifest, err := json.Marshal(captureMetadata{Configuration: config, Scope: ReadCapturedScope(worker, request, point)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := Snapshot{CaptureMode: CaptureVM, State: "ready", Hash: digest, VM: &VMProvenance{RuntimeCaptureID: capture}, Closure: &now, Manifest: manifest}
	summary := SummarizeCapture(snapshot)
	if summary.Configuration.Status != "verified" || summary.Configuration.SourceMachineRevision != 4 ||
		!strings.Contains(string(summary.Configuration.Spec), `"memory_mib":512`) || strings.Contains(string(summary.Configuration.Spec), "secret") ||
		strings.Contains(string(summary.Configuration.Spec), "idle_policy") {
		t.Fatalf("invalid captured configuration: %+v", summary.Configuration)
	}
	if summary.RAM != "included" || summary.RAMBytes == nil || *summary.RAMBytes != 12288 || len(summary.Disks) != 2 ||
		summary.Disks[0].GenerationID != "root-seal" || summary.Disks[0].ReadOnly || summary.Disks[1].GenerationID != "data-seal" ||
		!summary.Disks[1].ReadOnly || len(summary.RestorePolicies) != 2 {
		t.Fatalf("invalid capture scope: %+v", summary)
	}
	wrongEpoch := json.RawMessage(strings.ReplaceAll(string(worker), `"execution_epoch":7`, `"execution_epoch":8`))
	if ReadCapturedScope(wrongEpoch, request, point) != nil {
		t.Fatal("accepted record for a different execution")
	}
	wrongRoots := json.RawMessage(strings.ReplaceAll(string(worker), `["root-seal","data-seal"]`, `["root-seal","other"]`))
	if got := ReadCapturedScope(wrongRoots, request, point); got == nil || got.RAMBytes == 0 || got.Disks != nil {
		t.Fatalf("unproved disks: %+v", got)
	}
	snapshot.Manifest = json.RawMessage(`{"task_metadata_status":"stale"}`)
	summary = SummarizeCapture(snapshot)
	if summary.Configuration.Status != "unknown" || summary.RAM != "unknown" || summary.Disks != nil || len(summary.RestorePolicies) != 2 {
		t.Fatalf("historic snapshot: %+v", summary)
	}
}
