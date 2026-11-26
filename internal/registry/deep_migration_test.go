package registry_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
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
	files, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		prefix, _, _ := strings.Cut(file.Name(), "_")
		revision, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if revision > version {
			continue
		}
		body, err := migrations.Files.ReadFile(file.Name())
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
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES('m','vm','t',1,1,'paused','{}','{"enabled":true,"timeout_seconds":17}',0);
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
	if err != nil || control.SchemaVersion < 1020 || control.ControlEpoch != "control" {
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
	if err != nil || b.WorkerID != registry.LocalWorkerID || !b.BundleRoute().Legacy() || b.PowerState != "unknown" ||
		b.RuntimePauseID != "pause-v1" || b.PauseStatus != "active" || b.ObservedState != "paused" {
		t.Fatal(b, err)
	}
	idle, err := store.LifecyclePolicy(ctx, "m")
	if err != nil || idle.Source != "legacy" || idle.Desired.AutoStandby || !idle.Desired.AutoHibernate ||
		idle.Desired.HibernateAfter != 17 || idle.Desired.PressureReclaim != "disabled" || idle.AppliedRevision != 0 {
		t.Fatalf("legacy idle policy %+v, %v", idle, err)
	}
}

func TestMigrationReconfirmsOnlyImplicitLegacyPressurePolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platform.db")
	db := legacyRegistry(t, path, 1017)
	_, err := db.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,1017,'install','control');
		INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES('tpl','base',1,'image','{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id, source, desired, overrides, expectedPressure  string
		previousApplied, expectedDesired, expectedApplied int64
	}{
		{"applied", "legacy", `{"pressure_reclaim":"idle_only"}`, "{}", "disabled", 5, 6, 0},
		{"pending", "legacy", `{"pressure_reclaim":"idle_only"}`, "{}", "disabled", 4, 6, 0},
		{"explicit", "legacy", `{"pressure_reclaim":"idle_only"}`, `{"pressure_reclaim":"idle_only"}`, "idle_only", 5, 5, 5},
		{"disabled", "legacy", `{"pressure_reclaim":"disabled"}`, "{}", "disabled", 5, 5, 5},
		{"modern", "override", `{"pressure_reclaim":"idle_only"}`, "{}", "idle_only", 5, 5, 5},
	}
	for _, row := range tests {
		_, err = db.Exec(`INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
			VALUES(?,?,'tpl',1,1,'running','{}','{"enabled":true,"timeout_seconds":17}',0)`, row.id, row.id)
		if err == nil {
			_, err = db.Exec(`INSERT INTO machine_lifecycle_policies(
				machine_id,source,desired_revision,applied_revision,desired_json,overrides_json,applied_json,last_operation,worker_status)
				VALUES(?,?,5,?,?,?,?, 'original-op','{"power":"RUNNING"}')`,
				row.id, row.source, row.previousApplied, row.desired, row.overrides, row.desired)
		}
		if err != nil {
			t.Fatal(row.id, err)
		}
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
	for _, row := range tests {
		p, err := store.LifecyclePolicy(ctx, row.id)
		if err != nil || p.Source != row.source || p.LastOperation != "original-op" || p.DesiredRevision != row.expectedDesired ||
			p.AppliedRevision != row.expectedApplied || p.Desired.PressureReclaim != row.expectedPressure {
			t.Fatalf("%s: policy %+v, %v", row.id, p, err)
		}
		if row.expectedApplied == 0 && (p.Applied != nil || p.Worker != nil) {
			t.Fatalf("%s retained an acknowledgement for replaced policy: %+v", row.id, p)
		}
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

func TestHistoricalRegistryRollsBackFailedBundleSecurityMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platform.db")
	db := legacyRegistry(t, path, 2)
	_, err := db.Exec(`INSERT INTO meta(id,schema_version,installation_id,control_epoch) VALUES(1,2,'install','control');
		INSERT INTO principals(id,username,created_at) VALUES('historical','historical',1);
		CREATE TABLE runtime_bundle_security(injected_failure INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if store, err := registry.Open(ctx, path); err == nil {
		store.Close()
		t.Fatal("expected the final migration to fail")
	}
	var version, metadata, historical, earlierTables int
	for query, value := range map[string]*int{
		"PRAGMA user_version":                                             &version,
		"SELECT schema_version FROM meta WHERE id=1":                      &metadata,
		"SELECT COUNT(*) FROM principals WHERE id='historical'":           &historical,
		"SELECT COUNT(*) FROM sqlite_master WHERE name='runtime_bundles'": &earlierTables,
	} {
		if err := db.QueryRow(query).Scan(value); err != nil {
			t.Fatal(err)
		}
	}
	if version != 2 || metadata != 2 || historical != 1 || earlierTables != 0 {
		t.Fatalf("migration left partial effects: version=%d metadata=%d historical=%d tables=%d", version, metadata, historical, earlierTables)
	}
	if _, err := db.Exec("DROP TABLE runtime_bundle_security"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err := store.Control(ctx)
	if err != nil || control.SchemaVersion < 1014 || control.ControlEpoch != "control" {
		t.Fatal(control, err)
	}
}
