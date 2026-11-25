// Package maintenance handles administrator-initiated local backup and recovery.
package maintenance

import (
	"bytes"
	"context"
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
	"sandbox-platform/internal/snapshots"
)

const (
	manifestName = "backup-manifest.json"
	databaseName = "platform.db"
	configName   = "config.yaml"
)

type Backup struct {
	Format         int       `json:"format"`
	CreatedAt      time.Time `json:"created_at"`
	InstallationID string    `json:"installation_id"`
	ControlEpoch   string    `json:"control_epoch"`
	SchemaVersion  int       `json:"schema_version"`
	Files          []File    `json:"files"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func CreateBackup(ctx context.Context, cfg config.Config, configPath, output string) (Backup, error) {
	var result Backup
	if output == "" {
		return result, errors.New("backup output directory is required")
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(filepath.Dir(output), ".sandbox-backup-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	source, err := openBackupDB(cfg.Database.Path)
	if err != nil {
		return result, err
	}
	defer source.Close()
	if err := onlineCopy(ctx, filepath.Join(work, databaseName), source); err != nil {
		return result, err
	}
	db, err := openStandaloneDB(filepath.Join(work, databaseName))
	if err != nil {
		return result, err
	}
	defer db.Close()
	result = Backup{Format: 1, CreatedAt: time.Now().UTC()}
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

	artifacts, err := readyArtifacts(ctx, db)
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
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(work, manifestName), append(data, '\n'), 0600); err != nil {
		return result, err
	}
	if err := syncTree(work); err != nil {
		return result, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, work, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE); err != nil {
		return result, fmt.Errorf("publish backup directory: %w", err)
	}
	return result, syncPath(filepath.Dir(output))
}

func VerifyBackup(ctx context.Context, cfg config.Config, archive string) (Backup, error) {
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
	if result.Format != 1 || result.CreatedAt.IsZero() {
		return result, errors.New("unsupported backup format")
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
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return result, err
	}
	if integrity != "ok" {
		return result, errors.New("backup database failed SQLite integrity check")
	}
	artifacts, err := readyArtifacts(ctx, db)
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
	return result, verifyArtifacts(cfg, archive, artifacts)
}

var artifactFiles = []string{snapshots.ManifestFile, snapshots.SignatureFile, snapshots.CheckpointFile}

type readyArtifact struct {
	Path     string
	Digest   string
	Size     int64
	Manifest []byte
}

func readyArtifacts(ctx context.Context, db *sql.DB) ([]readyArtifact, error) {
	rows, err := db.QueryContext(ctx, "SELECT relative_path, sha256, size_bytes, manifest_json FROM snapshots WHERE state = 'ready' ORDER BY relative_path")
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
