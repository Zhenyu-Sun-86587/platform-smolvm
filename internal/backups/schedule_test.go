package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/maintenance"
	"sandbox-platform/internal/registry"
)

func TestFailedRunKeepsLastGoodArchive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.StateDir = root
	cfg.Database.Path = filepath.Join(root, "platform.db")
	cfg.Storage.KeyFile = filepath.Join(root, "signer.pem")
	policy := config.BackupPolicy{ID: "local", Scope: config.BackupPortable, Directory: filepath.Join(root, "backups"),
		IntervalSeconds: 3600, Retain: 1, RecoveryKeyFile: filepath.Join(root, "recovery-key")}
	cfg.Backups = []config.BackupPolicy{policy}
	configFile := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configFile, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy.RecoveryKeyFile, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := registry.Open(ctx, cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := runPolicy(ctx, db, cfg, configFile, policy, true); err != nil {
		t.Fatal(err)
	}
	runs, err := db.Backups(ctx, policy.ID)
	if err != nil || len(runs) != 1 || runs[0].State != "verified" {
		t.Fatalf("backup: %#v %v", runs, err)
	}
	lastGood := filepath.Join(policy.Directory, runs[0].ID)
	if err := os.Rename(policy.RecoveryKeyFile, policy.RecoveryKeyFile+".fixture"); err != nil {
		t.Fatal(err)
	}
	// Move the last run outside the interval so the scheduler attempts a new run.
	if _, err := db.DB().ExecContext(ctx, "UPDATE backup_runs SET started_at=? WHERE id=?", time.Now().Add(-2*time.Hour).UnixMilli(), runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := runPolicy(ctx, db, cfg, configFile, policy, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(policy.RecoveryKeyFile+".fixture", policy.RecoveryKeyFile); err != nil {
		t.Fatal(err)
	}
	runs, err = db.Backups(ctx, policy.ID)
	if err != nil || len(runs) != 2 || runs[0].FailureCode != "RECOVERY_KEY_UNAVAILABLE" || runs[1].State != "verified" {
		t.Fatalf("backup runs: %#v %v", runs, err)
	}
	if _, err := maintenance.VerifyBackup(ctx, cfg, lastGood); err != nil {
		t.Fatal(err)
	}
}
