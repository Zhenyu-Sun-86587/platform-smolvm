package registry_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	"sandbox-platform/migrations"
)

func legacyRegistry(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"001_registry.sql", "002_product.sql"}
	if version >= 1001 {
		files = append(files, "1001_deep_runtime.sql")
	}
	if version >= 1002 {
		files = append(files, "1002_deep_full_state.sql")
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
	return db
}

func TestDeepMigrationPreservesPauseEvidence(t *testing.T) {
	for _, version := range []int{2, 1001, 1002} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			testDeepMigrationPreservesPauseEvidence(t, version)
		})
	}
}

func testDeepMigrationPreservesPauseEvidence(t *testing.T, version int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform.db")
	db := legacyRegistry(t, path, version)
	_, err := db.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,?,'install','control');
		INSERT INTO principals (id, username, created_at) VALUES ('integration-service','service',1);
		INSERT INTO principals (id, username, password_hash, created_at) VALUES ('owner-id','owner','stored-hash',2);
		INSERT INTO events (control_epoch,type,payload_json,created_at,audit)
		VALUES ('control','auth','{"action":"owner_created","principal_id":"owner-id"}',2,1);
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
	if err != nil || control.SchemaVersion != 1007 || control.ControlEpoch != "control" {
		t.Fatal(control, err)
	}
	owner, err := auth.New(store).Bootstrap(ctx, "owner", "existing-password")
	if err != nil || owner.ID != "owner-id" {
		t.Fatal(owner, err)
	}
	if id, err := store.OwnerID(ctx); err != nil || id != owner.ID {
		t.Fatal(id, err)
	}
	b, err := store.GetBinding(ctx, "b")
	if err != nil || b.PowerState != "unknown" || b.RuntimeBundleID != "" ||
		b.RuntimePauseID != "pause-v1" || b.PauseStatus != "active" || b.ObservedState != "paused" {
		t.Fatal(b, err)
	}
}

func TestDeepMigrationKeepsAmbiguousLegacyOwnerUnassigned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platform.db")
	db := legacyRegistry(t, path, 2)
	digest := sha256.Sum256([]byte("legacy-test-secret"))
	_, err := db.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,2,'install','control');
		INSERT INTO principals(id,username,password_hash,created_at) VALUES
			('p_first','first','stored-hash-1',1), ('p_second','second','stored-hash-2',2);
		INSERT INTO credentials(id,principal_id,kind,digest,scope_json,created_at)
		VALUES ('token','p_second','api_token',?,'{"access":"read"}',2);`, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.OwnerID(ctx); !errors.Is(err, registry.ErrOwnerUnverified) {
		t.Fatal(err)
	}
	a := auth.New(store)
	if _, err := a.Bootstrap(ctx, "third", "password"); !errors.Is(err, registry.ErrOwnerUnverified) {
		t.Fatal(err)
	}
	var count int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM principals`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if p, err := a.Authenticate(ctx, "legacy-test-secret"); err != nil || p.ID != "p_second" || p.Scope.Access != auth.AccessRead {
		t.Fatal(p, err)
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
