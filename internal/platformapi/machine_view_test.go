package platformapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

func TestMachineViewSeparatesPowerHealthAndCapabilityEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.EnsureTemplate(ctx, registry.Template{ID: "t1", Name: "t", Version: 1, ImageDigest: "sha256:x", Capabilities: json.RawMessage(`{"live_fork":{"supported":true},"state.hibernate":{"supported":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	s := Server{Engine: &lifecycle.Engine{DB: db, Config: config.Defaults()}}
	now := time.Now().UTC()
	m := registry.Machine{TemplateID: "t1", Spec: json.RawMessage(`{}`), ObservedAt: &now, ObservedState: "paused", CurrentBinding: &registry.Binding{Observation: registry.Observation{GuestManagementStatus: "unknown"}}}
	v, err := s.viewMachine(ctx, m)
	if err != nil || v.PowerState != "unknown" || v.Capabilities["live_fork"].Status != "supported" || v.Capabilities["state.hibernate"].Supported {
		t.Fatal(v, err)
	}
	m.ObservedState = "running"
	m.CurrentBinding.RuntimeBundleID = "bundle"
	m.CurrentBinding.RuntimeInstanceID = "serve"
	m.CurrentBinding.PowerState = "running"
	m.CurrentBinding.Health = json.RawMessage(`{"vm":"running","guest_management":"unavailable","workload":"failed","workload_error":{"code":"WORKLOAD_START_FAILED","message":"entrypoint"}}`)
	v, err = s.viewMachine(ctx, m)
	if err != nil || v.PowerState != "running" || v.Health.Workload != "failed" || v.Health.GuestManagement != "unavailable" ||
		v.Capabilities["live_fork"].Supported || v.Capabilities["state.hibernate"].Status != "unknown" {
		t.Fatal(v, err)
	}
}
