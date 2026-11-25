package registry_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/registry"
	"sandbox-platform/migrations"
)

func TestDeepMigrationPreservesPauseEvidence(t *testing.T) {
	for _, version := range []int{2, 1001} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			testDeepMigrationPreservesPauseEvidence(t, version)
		})
	}
}

func testDeepMigrationPreservesPauseEvidence(t *testing.T, version int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"001_registry.sql", "002_product.sql"}
	if version == 1001 {
		files = append(files, "1001_deep_runtime.sql")
	}
	for _, name := range files {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,?,'install','control');
		INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES('t','base',1,'image','{}','{}');
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running) VALUES('m','vm','t',1,1,'paused','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,observed_state,runtime_pause_id,pause_status)
		VALUES('b','m',1,'vm','current','tasks','digest','paused','pause-v1','active')`, version)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err := store.Control(ctx)
	if err != nil || control.SchemaVersion != 1002 || control.ControlEpoch != "control" {
		t.Fatal(control, err)
	}
	b, err := store.GetBinding(ctx, "b")
	if err != nil || b.PowerState != "unknown" || b.RuntimeBundleID != "" ||
		b.RuntimePauseID != "pause-v1" || b.PauseStatus != "active" || b.ObservedState != "paused" {
		t.Fatal(b, err)
	}
}

func TestRejectOtherRouteSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := registry.Open(context.Background(), path); err == nil {
		store.Close()
		t.Fatal("accepted a Phase 2 registry")
	}
}
