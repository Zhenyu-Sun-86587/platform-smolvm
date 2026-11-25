package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func TestStoppedAfterHostRebootRequiresFreshNativeEvidence(t *testing.T) {
	binding := registry.Binding{Observation: registry.Observation{RuntimeIdentity: json.RawMessage(`{"pid":80,"start_time":10,"boot_id":"old"}`)}}
	for _, tt := range []struct {
		name, boot, state, raw, pause string
		want                          bool
	}{
		{"new host boot", "new", "stopped", "stopped", "", true},
		{"same host boot", "old", "stopped", "stopped", "", false},
		{"missing boot ID", "", "stopped", "stopped", "", false},
		{"absent process proof", "new", "unknown", "stopped", "", false},
		{"saved paused process", "new", "paused", "paused", "active", false},
		{"conflicting saved artifact", "new", "stopped", "stopped", "unknown", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binding.PauseStatus = tt.pause
			got := stoppedAfterHostReboot(tt.boot, binding, rt.Observation{State: tt.state, RawState: tt.raw})
			if got != tt.want {
				t.Fatalf("stopped-after-reboot evidence = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordHostBootLossKeepsTerminalReceipts(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const identity = `{"pid":80,"start_time":10,"boot_id":"old"}`
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO templates(id, name, version, image_digest, spec_json, capabilities_json)
			VALUES ('t', 't', 1, 'digest', '{}', '{}');
		INSERT INTO machines(id, name, template_id, revision, binding_generation, desired_power, spec_json, idle_policy_json, keep_running)
			VALUES ('m', 'm', 't', 1, 2, 'running', '{}', '{}', 0);
		INSERT INTO runtime_bindings(id, machine_id, generation, runtime_name, status, observed_state, runtime_identity_json, task_namespace, spec_hash)
			VALUES ('b', 'm', 2, 'native', 'current', 'running', ?, 'ns', 'hash');
		UPDATE machines SET current_binding_id = 'b' WHERE id = 'm';
		INSERT INTO tasks(id, machine_id, binding_generation, task_namespace, guest_task_id, start_token, spec_hash, spec_json, state, result_json)
			VALUES ('active', 'm', 2, 'ns', 'active', 'a', 'hash', '{}', 'running', '{"state":"running"}'),
				('done', 'm', 2, 'ns', 'done', 'b', 'hash', '{}', 'failed', '{"state":"failed"}'),
				('retired', 'm', 1, 'oldns', 'retired', 'c', 'hash', '{}', 'running', '{"state":"running"}');
	`, identity)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{DB: store}
	binding := registry.Binding{ID: "b", MachineID: "m", Generation: 2, TaskNamespace: "ns",
		Observation: registry.Observation{RuntimeIdentity: json.RawMessage(identity)}}
	if err := engine.recordTaskUncertainty(ctx, binding, time.Now(), "host_boot_changed"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"active": "unknown", "done": "failed", "retired": "running"} {
		var state, resultState string
		var reason *string
		err := store.ReadDB().QueryRowContext(ctx, `SELECT state, json_extract(result_json, '$.state'),
			json_extract(result_json, '$.reason') FROM tasks WHERE id=?`, id).Scan(&state, &resultState, &reason)
		if err != nil || state != want || resultState != want || (id == "active" && (reason == nil || *reason != "host_boot_changed")) {
			t.Fatalf("%s: state=%q receipt=%q reason=%v error=%v", id, state, resultState, reason, err)
		}
	}
}
