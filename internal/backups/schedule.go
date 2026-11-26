// Package backups schedules the configured installation backup policies.
package backups

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/maintenance"
	"sandbox-platform/internal/registry"
)

func Run(ctx context.Context, db *registry.Store, cfg config.Config, configPath string) {
	if len(cfg.Backups) == 0 {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var nextRetention time.Time
	for {
		checkRetention := !time.Now().Before(nextRetention)
		if checkRetention {
			nextRetention = time.Now().Add(time.Minute)
		}
		for _, policy := range cfg.Backups {
			if ctx.Err() != nil {
				return
			}
			if err := runPolicy(ctx, db, cfg, configPath, policy, checkRetention); err != nil && ctx.Err() == nil {
				slog.Warn("backup policy awaits reconciliation", "policy_id", policy.ID, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func runPolicy(ctx context.Context, db *registry.Store, cfg config.Config, configPath string, policy config.BackupPolicy, checkRetention bool) error {
	run, err := db.ClaimScheduledBackup(ctx, policy.ID, policy.Scope, time.Now().Add(-time.Duration(policy.IntervalSeconds)*time.Second))
	if err != nil {
		return err
	}
	if run.ID == "" {
		if checkRetention {
			return retireOld(ctx, db, cfg, policy, "")
		}
		return nil
	}
	control, err := db.Control(ctx)
	if err != nil {
		return err
	}
	if run.ControlEpoch != control.ControlEpoch || run.Scope != policy.Scope {
		if run.Scope == config.BackupNative {
			if err := maintenance.ReleaseNativeBackup(ctx, cfg, db, run.ID); err != nil {
				return err
			}
		}
		return db.FinishBackup(ctx, run, "failed", "BACKUP_SCOPE_CHANGED")
	}
	// The installation lock makes each run single-writer across daemon restarts.
	// An existing output may have been published before its receipt was committed.
	output := filepath.Join(policy.Directory, run.ID)
	if _, err = os.Lstat(output); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(policy.Directory, 0700); err != nil {
			return failRun(ctx, db, run, err)
		}
		_, err = maintenance.CreatePlannedBackup(ctx, cfg, configPath, output, maintenance.BackupPlan{
			ID: run.ID, Scope: run.Scope, RecoveryKeyFile: policy.RecoveryKeyFile,
			Registry:    db,
			CopyCatalog: func(copyCatalog func() error) error { return db.PinBackupCopy(ctx, run, copyCatalog) },
		})
	} else if err == nil {
		var archive maintenance.Backup
		archive, err = maintenance.VerifyPlannedBackup(ctx, cfg, output, policy.RecoveryKeyFile)
		if err == nil && (archive.RunID != run.ID || archive.Scope != run.Scope || archive.ControlEpoch != run.ControlEpoch || archive.InstallationID != control.InstallationID) {
			err = registry.ErrConflict
		}
		if err == nil {
			err = maintenance.CommitPublishedBackup(output)
		}
	}
	if errors.Is(err, maintenance.ErrBackupPublicationPending) || errors.Is(err, maintenance.ErrNativeBackupRetry) {
		return err
	}
	if ctx.Err() == nil && run.Scope == config.BackupNative {
		if releaseErr := maintenance.ReleaseNativeBackup(ctx, cfg, db, run.ID); releaseErr != nil {
			return releaseErr
		}
	}
	if err != nil {
		return failRun(ctx, db, run, err)
	}
	if err := db.FinishBackup(ctx, run, "verified", ""); err != nil {
		return err
	}
	return retireOld(ctx, db, cfg, policy, run.ID)
}

func failRun(ctx context.Context, db *registry.Store, run registry.BackupRun, failure error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	code := "BACKUP_VERIFY_FAILED"
	switch {
	case errors.Is(failure, maintenance.ErrNativeClosure):
		code = "NATIVE_CLOSURE_UNAVAILABLE"
	case errors.Is(failure, maintenance.ErrRecoveryKey):
		code = "RECOVERY_KEY_UNAVAILABLE"
	case errors.Is(failure, registry.ErrControlEpoch):
		code = "CONTROL_EPOCH_CHANGED"
	}
	slog.Warn("backup failed", "backup_id", run.ID, "failure_code", code, "error", failure)
	return db.FinishBackup(ctx, run, "failed", code)
}

func retireOld(ctx context.Context, db *registry.Store, cfg config.Config, policy config.BackupPolicy, justVerified string) error {
	control, err := db.Control(ctx)
	if err != nil || control.Maintenance {
		return err
	}
	runs, err := db.Backups(ctx, policy.ID)
	if err != nil {
		return err
	}
	runs = slices.DeleteFunc(runs, func(run registry.BackupRun) bool {
		return run.Scope != policy.Scope || run.State != "verified" && run.State != "retiring"
	})
	if len(runs) <= policy.Retain {
		return nil
	}
	verified := func(run registry.BackupRun) bool {
		if run.ID == justVerified {
			return true
		}
		metadata, err := maintenance.VerifyPlannedBackup(ctx, cfg, filepath.Join(policy.Directory, run.ID), policy.RecoveryKeyFile)
		return err == nil && metadata.RunID == run.ID && metadata.Scope == run.Scope &&
			metadata.ControlEpoch == run.ControlEpoch && metadata.InstallationID == control.InstallationID
	}
	kept := 0
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if kept < policy.Retain {
			if run.State == "verified" && verified(run) {
				kept++
			}
			continue
		}
		path := filepath.Join(policy.Directory, run.ID)
		if run.State == "verified" {
			if !verified(run) {
				continue
			}
			if err := db.FinishBackup(ctx, run, "retiring", ""); err != nil {
				return err
			}
			run.State = "retiring"
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		parent, err := os.Open(policy.Directory)
		if err != nil {
			return err
		}
		if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
			return err
		}
		if err := db.FinishBackup(ctx, run, "retired", ""); err != nil {
			return err
		}
	}
	return nil
}
