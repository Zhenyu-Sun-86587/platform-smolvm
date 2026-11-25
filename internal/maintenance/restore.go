package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
)

type Restore struct {
	ArchiveEpoch string           `json:"archive_epoch"`
	Before       Backup           `json:"previous_backup"`
	Control      registry.Control `json:"control"`
}

// RestoreBackup requires the active writer to be stopped and the target to
// belong to the same installation. It restores only platform-owned data.
func RestoreBackup(ctx context.Context, cfg config.Config, configPath, archive, rollback string) (Restore, error) {
	var result Restore
	if rollback == "" {
		return result, errors.New("a new directory for the pre-restore backup is required")
	}
	live, err := registry.Open(ctx, cfg.Database.Path)
	if err != nil {
		return result, err
	}
	defer live.Close()
	control, err := live.Control(ctx)
	if err != nil {
		return result, err
	}
	if !control.Maintenance {
		return result, errors.New("enter local maintenance before restoring")
	}
	metadata, err := VerifyBackup(ctx, cfg, archive)
	if err != nil {
		return result, err
	}
	result.ArchiveEpoch = metadata.ControlEpoch
	if metadata.InstallationID != control.InstallationID {
		return result, errors.New("backup belongs to a different installation")
	}
	if metadata.SchemaVersion > control.SchemaVersion {
		return result, errors.New("backup schema is newer than this platform release")
	}

	stage, err := os.MkdirTemp(cfg.StateDir, ".sandbox-restore-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	for _, expected := range metadata.Files {
		if expected.Path == configName {
			continue
		}
		actual, err := digestFile(archive, expected.Path, filepath.Join(stage, expected.Path))
		if err != nil {
			return result, err
		}
		if actual != expected {
			return result, fmt.Errorf("backup changed during restore: %s", expected.Path)
		}
	}
	staged, err := registry.Open(ctx, filepath.Join(stage, databaseName))
	if err != nil {
		return result, err
	}
	defer staged.Close()
	result.Control, err = staged.BeginRecovery(ctx, "database backup restored from epoch "+metadata.ControlEpoch)
	if err != nil {
		return result, err
	}
	if err := staged.MarkRestoredHistory(ctx); err != nil {
		return result, err
	}
	artifacts, err := readyArtifacts(ctx, staged.ReadDB())
	if err != nil {
		return result, err
	}
	result.Before, err = CreateBackup(ctx, cfg, configPath, rollback)
	if err != nil {
		return result, fmt.Errorf("preserve current installation: %w", err)
	}

	uid, gid, err := databaseOwner(cfg.Database.Path)
	if err != nil {
		return result, err
	}
	entries := make(map[string]File)
	for _, file := range metadata.Files {
		entries[file.Path] = file
	}
	if err := syncTree(stage); err != nil {
		return result, err
	}
	for _, artifact := range artifacts {
		path := artifact.Path
		dir := filepath.Join(cfg.StateDir, path)
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(dir)
			if err := os.MkdirAll(parent, 0700); err != nil {
				return result, err
			}
			if err := os.Chown(parent, uid, gid); err != nil {
				return result, err
			}
			from := filepath.Join(stage, path)
			if err := filepath.WalkDir(from, func(p string, _ os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Chown(p, uid, gid)
			}); err != nil {
				return result, err
			}
			if err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, dir, unix.RENAME_NOREPLACE); err != nil {
				return result, err
			}
			if err := syncPath(parent); err != nil {
				return result, err
			}
		} else if err != nil {
			return result, err
		} else {
			for _, name := range artifactFiles {
				relative := filepath.Join(path, name)
				actual, err := digestFile(cfg.StateDir, relative, "")
				if err != nil {
					return result, err
				}
				if actual != entries[relative] {
					return result, fmt.Errorf("existing artifact differs from backup: %s", path)
				}
			}
		}
	}
	if err := copyDatabase(ctx, live.DB(), staged.ReadDB()); err != nil {
		return result, fmt.Errorf("restore registry while holding its writer lock: %w", err)
	}
	return result, nil
}

func databaseOwner(path string) (uid, gid int, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	owner := info.Sys().(*syscall.Stat_t)
	return int(owner.Uid), int(owner.Gid), nil
}

func SetMaintenance(ctx context.Context, cfg config.Config, enabled bool, reason string) (registry.Control, error) {
	if _, err := os.Stat(cfg.Database.Path); err != nil {
		return registry.Control{}, err
	}
	store, err := registry.Open(ctx, cfg.Database.Path)
	if err != nil {
		return registry.Control{}, err
	}
	defer store.Close()
	if enabled {
		reason = strings.TrimSpace(reason)
	}
	return store.SetMaintenance(ctx, enabled, reason)
}

func CurrentControl(ctx context.Context, cfg config.Config) (registry.Control, error) {
	db, err := openBackupDB(cfg.Database.Path)
	if err != nil {
		return registry.Control{}, err
	}
	defer db.Close()
	var control registry.Control
	err = db.QueryRowContext(ctx, `SELECT schema_version, installation_id, control_epoch, maintenance,
		COALESCE(recovery_reason, '') FROM meta WHERE id = 1`).Scan(&control.SchemaVersion, &control.InstallationID,
		&control.ControlEpoch, &control.Maintenance, &control.RecoveryReason)
	return control, err
}
