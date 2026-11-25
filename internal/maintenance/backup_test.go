package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
)

func TestBackupRestoreChangesEpochWithoutReplayingOldWork(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.StateDir = root
	cfg.Database.Path = filepath.Join(root, databaseName)
	configPath := filepath.Join(root, "live.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(ctx, cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO operations (id, control_epoch, binding_generation, kind, state, stage, request_json, effect,
		created_at, updated_at, heavy) VALUES ('op_old', ?, 0, 'create', 'queued', 'queued', '{}', 'none', 1, 1, 1)`, control.ControlEpoch)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "backup")
	backup, err := CreateBackup(ctx, cfg, configPath, archive)
	if err != nil || backup.ControlEpoch != control.ControlEpoch {
		t.Fatalf("online backup = %+v, %v", backup, err)
	}
	if _, err := VerifyBackup(ctx, cfg, archive); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(root, "pre-restore")
	if _, err := RestoreBackup(ctx, cfg, configPath, archive, rollback); !errors.Is(err, registry.ErrLocked) {
		t.Fatalf("restore while single writer active = %v", err)
	}
	_, err = store.DB().ExecContext(ctx, "UPDATE operations SET state='failed' WHERE id='op_old'")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := SetMaintenance(ctx, cfg, true, "integration restore"); err != nil {
		t.Fatal(err)
	}
	result, err := RestoreBackup(ctx, cfg, configPath, archive, rollback)
	if err != nil || result.Control.ControlEpoch == control.ControlEpoch || !result.Control.Maintenance {
		t.Fatalf("restore = %+v, %v", result, err)
	}
	store, err = registry.Open(ctx, cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state, effect string
	if err := store.ReadDB().QueryRowContext(ctx, "SELECT state, effect FROM operations WHERE id='op_old'").Scan(&state, &effect); err != nil {
		t.Fatal(err)
	}
	if state != "recovery_required" || effect != "unknown" {
		t.Fatalf("restored operation is %s, %s", state, effect)
	}
	if _, err := VerifyBackup(ctx, cfg, rollback); err != nil {
		t.Fatal(err)
	}
}
