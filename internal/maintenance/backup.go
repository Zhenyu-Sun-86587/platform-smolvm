// Package maintenance handles administrator-initiated local backup and recovery.
package maintenance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/snapshots"
)

const (
	manifestName = "backup-manifest.json"
	databaseName = "platform.db"
	configName   = "config.yaml"
)

type Backup struct {
	Format         int             `json:"format"`
	CreatedAt      time.Time       `json:"created_at"`
	InstallationID string          `json:"installation_id"`
	ControlEpoch   string          `json:"control_epoch"`
	SchemaVersion  int             `json:"schema_version"`
	Files          []File          `json:"files"`
	RunID          string          `json:"backup_id,omitempty"`
	Scope          string          `json:"scope,omitempty"`
	MAC            string          `json:"manifest_mac,omitempty"`
	Native         []NativeArchive `json:"native_closures,omitempty"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func CreateBackup(ctx context.Context, cfg config.Config, configPath, output string) (Backup, error) {
	return createBackup(ctx, cfg, configPath, output, nil)
}

type BackupPlan struct {
	ID, Scope, RecoveryKeyFile string
	CopyCatalog                func(func() error) error
	Registry                   *registry.Store
}

var ErrNativeClosure = errors.New("native backup closure is unavailable")
var ErrBackupPublicationPending = errors.New("published backup directory is awaiting a durable commit")

func CreatePlannedBackup(ctx context.Context, cfg config.Config, configPath, output string, plan BackupPlan) (Backup, error) {
	return createBackup(ctx, cfg, configPath, output, &plan)
}

func createBackup(ctx context.Context, cfg config.Config, configPath, output string, plan *BackupPlan) (result Backup, err error) {
	if output == "" {
		return result, errors.New("backup output directory is required")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return result, err
	}
	work, copied, cleanup, err := backupStage(output, plan)
	if err != nil {
		return result, err
	}
	defer func() {
		if plan == nil || ctx.Err() == nil && !errors.Is(err, ErrNativeBackupRetry) {
			cleanup()
		}
	}()
	native := plan != nil && plan.Scope == config.BackupNative
	copyCatalog := func() error {
		source, err := openBackupDB(cfg.Database.Path)
		if err != nil {
			return err
		}
		return errors.Join(onlineCopy(ctx, filepath.Join(work, databaseName), source), source.Close())
	}
	sealCatalog := func() error {
		if err := syncPath(filepath.Join(work, databaseName)); err != nil {
			return err
		}
		if err := os.WriteFile(work+".catalog-ready", nil, 0600); err != nil {
			return err
		}
		return syncPath(filepath.Dir(work))
	}
	if native {
		if plan.Registry == nil {
			return result, errors.New("native backup requires the durable run registry")
		}
		if err := sealNativeCatalog(ctx, cfg, plan, work, copied, copyCatalog, sealCatalog); err != nil {
			return result, err
		}
	} else if !copied {
		if plan != nil && plan.CopyCatalog != nil {
			err = plan.CopyCatalog(copyCatalog)
		} else {
			err = copyCatalog()
		}
		if err != nil {
			return result, err
		}
		if plan != nil {
			if err := sealCatalog(); err != nil {
				return result, err
			}
		}
	}
	db, err := openStandaloneDB(filepath.Join(work, databaseName))
	if err != nil {
		return result, err
	}
	defer db.Close()
	var pinned []pinnedNative
	if native {
		pinned, err = pinNative(ctx, db, cfg, plan)
		if err != nil {
			return result, err
		}
	}
	result = Backup{Format: 1, CreatedAt: time.Now().UTC()}
	if plan != nil {
		result.Format, result.RunID, result.Scope = 2, plan.ID, plan.Scope
		if err := validatePortableScope(ctx, db, result.Scope); err != nil {
			return result, err
		}
	}
	if err := readMetadata(ctx, db, &result); err != nil {
		return result, err
	}
	dbFile, err := digestFile(work, databaseName, "")
	if err != nil {
		return result, err
	}
	cfgFile, err := digestFile(filepath.Dir(configPath), filepath.Base(configPath), filepath.Join(work, configName))
	if err != nil {
		return result, err
	}
	cfgFile.Path = configName
	result.Files = append(result.Files, cfgFile, dbFile)
	if plan != nil {
		recovery, err := exportRecovery(ctx, db, cfg, work, *plan)
		if err != nil {
			return result, err
		}
		result.Files = append(result.Files, recovery...)
		if native {
			var files []File
			result.Native, files, err = exportNative(ctx, work, pinned)
			if err != nil {
				return result, err
			}
			result.Files = append(result.Files, files...)
		}
	}

	artifacts, err := readyArtifactsFor(ctx, db, result.Scope == config.BackupNative)
	if err != nil {
		return result, err
	}
	for _, artifact := range artifacts {
		for _, name := range artifactFiles {
			file, err := digestFile(cfg.StateDir, filepath.Join(artifact.Path, name), filepath.Join(work, artifact.Path, name))
			if err != nil {
				return result, err
			}
			result.Files = append(result.Files, file)
		}
	}
	if err := verifyArtifacts(cfg, work, artifacts); err != nil {
		return result, err
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	if plan != nil {
		result.MAC, err = backupMAC(result, plan.RecoveryKeyFile)
		if err != nil {
			return result, err
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(work, manifestName), append(data, '\n'), 0600); err != nil {
		return result, err
	}
	if plan != nil {
		if _, err := VerifyPlannedBackup(ctx, cfg, work, plan.RecoveryKeyFile); err != nil {
			return result, err
		}
	}
	if err := syncTree(work); err != nil {
		return result, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, work, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE); err != nil {
		return result, fmt.Errorf("publish backup directory: %w", err)
	}
	if plan != nil {
		return result, CommitPublishedBackup(output)
	}
	return result, syncPath(filepath.Dir(output))
}

// CommitPublishedBackup completes the directory rename and removes the staging
// marker. It can run again after the daemon exits between rename and receipt.
func CommitPublishedBackup(output string) error {
	parent := filepath.Dir(output)
	marker := filepath.Join(parent, ".sandbox-"+filepath.Base(output)+".catalog-ready")
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrBackupPublicationPending, err)
	}
	if err := syncPath(parent); err != nil {
		return errors.Join(ErrBackupPublicationPending, err)
	}
	return nil
}

func backupStage(output string, plan *BackupPlan) (string, bool, func(), error) {
	parent := filepath.Dir(output)
	if plan == nil {
		stage, err := os.MkdirTemp(parent, ".sandbox-backup-")
		return stage, false, func() { _ = os.RemoveAll(stage) }, err
	}
	if filepath.Base(output) != plan.ID || !strings.HasPrefix(plan.ID, "backup_") {
		return "", false, nil, errors.New("planned backup path differs from the durable run ID")
	}
	stage := filepath.Join(parent, ".sandbox-"+plan.ID)
	marker := stage + ".catalog-ready"
	cleanup := func() { _ = os.RemoveAll(stage); _ = os.Remove(marker) }
	if info, err := os.Lstat(stage); err == nil && !info.IsDir() {
		return "", false, nil, errors.New("invalid existing backup stage")
	}
	entries, stageErr := os.ReadDir(stage)
	if stageErr != nil && !errors.Is(stageErr, os.ErrNotExist) {
		return "", false, nil, stageErr
	}
	info, markerErr := os.Lstat(marker)
	if stageErr == nil && markerErr == nil && info.Mode().IsRegular() {
		for _, entry := range entries {
			if entry.Name() != databaseName {
				if err := os.RemoveAll(filepath.Join(stage, entry.Name())); err != nil {
					return "", false, nil, err
				}
			}
		}
		return stage, true, cleanup, nil
	}
	cleanup()
	return stage, false, cleanup, os.Mkdir(stage, 0700)
}

func VerifyBackup(ctx context.Context, cfg config.Config, archive string) (Backup, error) {
	return verifyBackup(ctx, cfg, archive, "")
}

func VerifyPlannedBackup(ctx context.Context, cfg config.Config, archive, keyFile string) (Backup, error) {
	return verifyBackup(ctx, cfg, archive, keyFile)
}

func verifyBackup(ctx context.Context, cfg config.Config, archive, keyFile string) (Backup, error) {
	var result Backup
	if archive == "" {
		return result, errors.New("backup directory is required")
	}
	metadata, err := openFile(archive, manifestName)
	if err != nil {
		return result, err
	}
	defer metadata.Close()
	d := json.NewDecoder(io.LimitReader(metadata, 8<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return result, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return result, errors.New("invalid trailing backup metadata")
	}
	if (result.Format != 1 && result.Format != 2) || result.CreatedAt.IsZero() ||
		(result.Format == 2 && (result.RunID == "" || result.Scope == "")) {
		return result, errors.New("unsupported backup format")
	}
	if result.Format == 2 {
		keyFile = recoveryKeyPath(cfg, archive, result, keyFile)
		want, err := backupMAC(result, keyFile)
		if err != nil || !hmac.Equal([]byte(want), []byte(result.MAC)) {
			return result, ErrRecoveryKey
		}
	}
	verified := make(map[string]File)
	for _, expected := range result.Files {
		if _, ok := verified[expected.Path]; ok {
			return result, errors.New("duplicate backup file")
		}
		actual, err := digestFile(archive, expected.Path, "")
		if err != nil {
			return result, err
		}
		if actual != expected {
			return result, fmt.Errorf("backup file digest or length mismatch for %s", expected.Path)
		}
		verified[actual.Path] = actual
	}
	if _, ok := verified[databaseName]; !ok {
		return result, errors.New("backup manifest has no database")
	}
	if _, ok := verified[configName]; !ok {
		return result, errors.New("backup manifest has no configuration")
	}
	db, err := openStandaloneDB(filepath.Join(archive, databaseName))
	if err != nil {
		return result, err
	}
	defer db.Close()
	var actual Backup
	if err := readMetadata(ctx, db, &actual); err != nil {
		return result, err
	}
	if actual.InstallationID != result.InstallationID || actual.ControlEpoch != result.ControlEpoch || actual.SchemaVersion != result.SchemaVersion {
		return result, errors.New("database metadata differs from backup manifest")
	}
	var recovered map[string][]byte
	if result.Format == 2 {
		if err := validatePortableScope(ctx, db, result.Scope); err != nil {
			return result, err
		}
		recovered, err = verifyRecovery(archive, result.RunID, recoveryKeyPath(cfg, archive, result, keyFile), verified)
		if err != nil {
			return result, err
		}
		if err := verifyTaskRecovery(ctx, db, recovered); err != nil {
			return result, err
		}
		if result.Scope == config.BackupNative {
			if err := verifyNative(ctx, db, cfg, archive, result, verified); err != nil {
				return result, err
			}
		} else if len(result.Native) != 0 {
			return result, ErrNativeClosure
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return result, err
	}
	if integrity != "ok" {
		return result, errors.New("backup database failed SQLite integrity check")
	}
	artifacts, err := readyArtifactsFor(ctx, db, result.Scope == config.BackupNative)
	if err != nil {
		return result, err
	}
	for _, artifact := range artifacts {
		for _, name := range artifactFiles {
			if _, ok := verified[filepath.Join(artifact.Path, name)]; !ok {
				return result, fmt.Errorf("backup manifest has no file %s/%s", artifact.Path, name)
			}
		}
	}
	if len(verified) != 2+len(artifacts)*len(artifactFiles) {
		return result, errors.New("unexpected files listed in backup manifest")
	}
	if len(artifacts) == 0 {
		return result, nil
	}
	trust, err := snapshots.LoadRecoveryTrust(cfg, recovered["signer"])
	if err != nil {
		return result, err
	}
	return result, verifySignedArtifacts(trust, archive, artifacts)
}

func validatePortableScope(ctx context.Context, db *sql.DB, scope string) error {
	if scope == config.BackupNative {
		return nil
	}
	if scope != config.BackupPortable {
		return ErrNativeClosure
	}
	var native bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM snapshots WHERE state='ready' AND provenance='worker')
		OR EXISTS(SELECT 1 FROM runtime_bindings WHERE status='current' AND power_state='hibernated' AND runtime_bundle_id IS NOT NULL)
		OR EXISTS(SELECT 1 FROM volumes WHERE state='available')`).Scan(&native)
	if err == nil && native {
		return ErrNativeClosure
	}
	return err
}

var artifactFiles = []string{snapshots.ManifestFile, snapshots.SignatureFile, snapshots.CheckpointFile}

type readyArtifact struct {
	Path     string
	Digest   string
	Size     int64
	Manifest []byte
}

func readyArtifacts(ctx context.Context, db *sql.DB) ([]readyArtifact, error) {
	return readyArtifactsFor(ctx, db, false)
}

func readyArtifactsFor(ctx context.Context, db *sql.DB, separateNative bool) ([]readyArtifact, error) {
	rows, err := db.QueryContext(ctx, "SELECT relative_path,sha256,size_bytes,manifest_json FROM snapshots WHERE state='ready' AND (NOT ? OR provenance<>'worker') ORDER BY relative_path", separateNative)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []readyArtifact
	for rows.Next() {
		var artifact readyArtifact
		if err := rows.Scan(&artifact.Path, &artifact.Digest, &artifact.Size, &artifact.Manifest); err != nil {
			return nil, err
		}
		path := artifact.Path
		if !filepath.IsLocal(path) || path == "." || filepath.Clean(path) != path || strings.ContainsRune(path, filepath.Separator) {
			return nil, errors.New("snapshot has invalid artifact path")
		}
		artifact.Path = filepath.Join("artifacts", path)
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func readMetadata(ctx context.Context, db *sql.DB, backup *Backup) error {
	return db.QueryRowContext(ctx, "SELECT installation_id, control_epoch, schema_version FROM meta WHERE id = 1").
		Scan(&backup.InstallationID, &backup.ControlEpoch, &backup.SchemaVersion)
}

func verifyArtifacts(cfg config.Config, archive string, artifacts []readyArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	keys, err := snapshots.LoadTrust(cfg)
	if err != nil {
		return err
	}
	return verifySignedArtifacts(keys, archive, artifacts)
}

func verifySignedArtifacts(keys map[string]ed25519.PublicKey, archive string, artifacts []readyArtifact) error {
	for _, artifact := range artifacts {
		verified, err := snapshots.VerifyDirectory(filepath.Join(archive, artifact.Path), keys)
		if err != nil {
			return fmt.Errorf("verify %s: %w", artifact.Path, err)
		}
		if verified.Manifest.Digest != artifact.Digest || verified.Manifest.Size != artifact.Size || !bytes.Equal(verified.Raw, artifact.Manifest) {
			return fmt.Errorf("snapshot catalog differs from signed artifact %s", artifact.Path)
		}
	}
	return nil
}

func openFile(root, relative string) (*os.File, error) {
	if !filepath.IsLocal(relative) || relative == "." || filepath.Clean(relative) != relative {
		return nil, errors.New("invalid relative backup path")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, errors.New("backup member must have no symbolic links")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("backup member must be a regular file")
	}
	return f, nil
}

func digestFile(root, relative, destination string) (File, error) {
	result := File{Path: relative}
	f, err := openFile(root, relative)
	if err != nil {
		return result, err
	}
	defer f.Close()
	h := sha256.New()
	var output io.Writer = h
	if destination != "" {
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return result, err
		}
		dest, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return result, err
		}
		defer dest.Close()
		output = io.MultiWriter(h, dest)
	}
	result.Bytes, err = io.Copy(output, f)
	result.SHA256 = hex.EncodeToString(h.Sum(nil))
	return result, err
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		return syncPath(path)
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncPath(directories[i]); err != nil {
			return err
		}
	}
	return nil
}
