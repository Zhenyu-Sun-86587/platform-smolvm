package platformapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/servicehealth"
)

func TestPolishFixturesPreserveRetryIdentityAndUploadEffect(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/polish-p0.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Operations map[string]registry.Operation `json:"operations"`
		Files      struct {
			Download string                       `json:"download_base64"`
			Unknown  map[string]map[string]string `json:"unknown"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	original, retry := fixture.Operations["partial"], fixture.Operations["retry_slot"]
	if len(original.Children) != 2 || len(retry.Children) != 1 || original.ID != retry.ID || retry.State != registry.Observing ||
		original.Effect != registry.EffectPartial || original.Children[0].State != "ready" {
		t.Fatalf("native retry fixture mismatch: original %+v, retry %+v", original, retry)
	}
	prior, attempted := original.Children[1], retry.Children[0]
	if prior.State != "quarantined" || prior.ErrorCode != "NATIVE_CHILD_ABORTED" || prior.Attempt != 0 ||
		attempted.State != "pending" || attempted.Attempt != prior.Attempt+1 || attempted.Ordinal != prior.Ordinal ||
		attempted.MachineID != prior.MachineID || attempted.BindingID != prior.BindingID {
		t.Fatalf("native retry fixture changed the logical target: %+v -> %+v", prior, attempted)
	}
	if got, err := base64.StdEncoding.DecodeString(fixture.Files.Download); err != nil || len(got) != 4 || got[0] != 0 || got[1] != 255 {
		t.Fatalf("binary download fixture invalid: %x, %v", got, err)
	}
	rec := httptest.NewRecorder()
	fail(rec, httptest.NewRequest(http.MethodPut, "/v1/machines/m/files", nil), uploadUnknown())
	var actual map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &actual); err != nil || rec.Code != http.StatusConflict {
		t.Fatalf("upload uncertainty returned %d %s, %v", rec.Code, rec.Body.String(), err)
	}
	for key, expected := range fixture.Files.Unknown["error"] {
		if actual["error"][key] != expected {
			t.Errorf("upload %s = %q, want %q", key, actual["error"][key], expected)
		}
	}
}

func TestPolishPassiveViewsRejectOldExecution(t *testing.T) {
	s := &Server{}
	b := &registry.Binding{
		ID: "binding", Generation: 4, RuntimeInstanceID: "runtime", RuntimeBundleID: "bundle",
		RuntimeInstanceObjectID: "vm", IncarnationID: "incarn", ExecutionEpoch: 7, PowerState: registry.PowerRunning,
	}
	b.GuestManagementEvidence = lifecycle.JSON(map[string]any{
		"file_transfer": rt.GuestFileEvidence{Execution: b.ExecutionRef(), Supported: true},
	})
	view := machineView{BootMode: rt.BootSystem, Binding: b, Capabilities: map[string]rt.Capability{}}
	s.projectFileCapabilities(&view, false)
	if cap := view.Capabilities[rt.FileWriteCapability]; !cap.Supported {
		t.Fatalf("existing verified evidence absent: %+v", cap)
	}
	v := service{ID: "service", Enabled: true, Revision: 2}
	s.serviceHealth.Record(v.ID, serviceObservationIdentity(v, b), servicehealth.Diagnose(servicehealth.StageWorkerUpstream, nil))
	s.projectServiceObservation(&v, registry.Machine{CurrentBinding: b})
	if v.Observation == nil || v.Observation.Status != "ready" || v.Observation.Scope != "worker_upstream" {
		t.Fatalf("scope evidence absent: %+v", v.Observation)
	}
	b.ExecutionEpoch++
	s.projectFileCapabilities(&view, false)
	if cap := view.Capabilities[rt.FileWriteCapability]; cap.Supported || cap.Status != "unknown" {
		t.Fatalf("replaced execution reused obsolete capability: %+v", cap)
	}
	s.projectServiceObservation(&v, registry.Machine{CurrentBinding: b})
	if v.Observation.Status != "stale" || v.Observation.ExecutionEpoch != b.ExecutionEpoch-1 {
		t.Fatalf("replaced execution reused obsolete service observation: %+v", v.Observation)
	}
}
