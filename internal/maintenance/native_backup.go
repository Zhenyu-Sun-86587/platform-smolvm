package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/runtime/customsmolvm"
	"sandbox-platform/internal/runtime/workerclient"
)

type NativeArchive struct {
	RuntimeInstance string `json:"runtime_instance_id"`
	Bundle          string `json:"bundle_digest"`
	Reader          string `json:"state_reader_id"`
	Directory       string `json:"directory"`
	Digest          string `json:"manifest_digest"`
}

type nativeSelection struct {
	Route       customsmolvm.Route
	Captures    map[string]string
	Generations []string
}

type closureManifest struct {
	Schema   string           `json:"schema"`
	ID       string           `json:"backup_id"`
	Bundle   string           `json:"bundle_digest"`
	Reader   string           `json:"state_reader_id"`
	Members  map[string]int64 `json:"members"`
	Captures []struct {
		Point struct {
			ID string `json:"capture_id"`
		} `json:"point"`
		Digest string `json:"manifest_digest"`
	} `json:"captures"`
	Generations []string `json:"generations"`
}

type installedBundle struct {
	Artifacts []struct {
		Path   string `json:"path"`
		Length int64  `json:"size"`
		SHA    string `json:"sha256"`
	} `json:"artifacts"`
}

func routeSuffix(route customsmolvm.Route) string {
	digest := sha256.Sum256([]byte(route.InstanceID + "\x00" + route.Bundle))
	return fmt.Sprintf("%x", digest[:12])
}

func nativePlans(ctx context.Context, db *sql.DB, cfg config.Config) ([]nativeSelection, error) {
	routes := make(map[string]customsmolvm.Route)
	for _, route := range cfg.Runtime.Custom {
		routes[route.InstanceID] = route
	}
	selected := make(map[string]*nativeSelection)
	rows, err := db.QueryContext(ctx, `SELECT p.runtime_instance_id,p.runtime_bundle_id,p.runtime_capture_id,s.sha256,'capture'
		FROM vm_snapshot_provenance p JOIN snapshots s ON s.id=p.snapshot_id WHERE s.state='ready'
		UNION SELECT b.runtime_instance_id,b.runtime_bundle_id,p.capture_id,p.manifest_digest,'capture'
		FROM runtime_bindings b JOIN managed_statepoint_receipts p ON p.runtime_instance_id=b.runtime_instance_id AND p.capture_id=b.last_capture_id
		WHERE b.status='current' AND b.power_state='hibernated'
		UNION SELECT v.runtime_instance_id,v.runtime_bundle_id,s.generation_handle,'','disk'
		FROM volume_snapshots s JOIN volumes v ON s.volume_id=v.id WHERE s.state='available'
		ORDER BY 1,3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var routeID, bundle, id, digest, kind string
		if err := rows.Scan(&routeID, &bundle, &id, &digest, &kind); err != nil {
			return nil, err
		}
		route, ok := routes[routeID]
		if !ok || route.Bundle != bundle || route.WorkerSocket == "" {
			return nil, fmt.Errorf("%w: endpoint for the pinned bundle is unavailable", ErrNativeClosure)
		}
		if selected[routeID] == nil {
			selected[routeID] = &nativeSelection{Route: route, Captures: map[string]string{}, Generations: []string{}}
		}
		set := selected[routeID]
		if kind == "capture" {
			if old, existing := set.Captures[id]; existing && old != digest {
				return nil, fmt.Errorf("%w: StatePoint receipts disagree", ErrNativeClosure)
			}
			set.Captures[id] = digest
		} else if !slices.Contains(set.Generations, id) {
			set.Generations = append(set.Generations, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]nativeSelection, 0, len(selected))
	for _, set := range selected {
		result = append(result, *set)
	}
	slices.SortFunc(result, func(a, b nativeSelection) int { return strings.Compare(a.Route.InstanceID, b.Route.InstanceID) })
	return result, nil
}

func exportNative(ctx context.Context, db *sql.DB, cfg config.Config, stage, runID string) ([]NativeArchive, []File, error) {
	plans, err := nativePlans(ctx, db, cfg)
	if err != nil {
		return nil, nil, err
	}
	var archives []NativeArchive
	var files []File
	for _, plan := range plans {
		archive, members, err := exportNativeRoute(ctx, stage, runID, plan)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrNativeClosure, err)
		}
		archives = append(archives, archive)
		files = append(files, members...)
	}
	return archives, files, nil
}

func exportNativeRoute(ctx context.Context, stage, runID string, plan nativeSelection) (NativeArchive, []File, error) {
	suffix := routeSuffix(plan.Route)
	root := filepath.Join("native", suffix)
	archive := NativeArchive{RuntimeInstance: plan.Route.InstanceID, Bundle: plan.Route.Bundle, Reader: plan.Route.Reader, Directory: root}
	captures := make([]string, 0, len(plan.Captures))
	for id := range plan.Captures {
		captures = append(captures, id)
	}
	slices.Sort(captures)
	client := workerclient.Client{Socket: plan.Route.WorkerSocket, Bundle: plan.Route.Bundle}
	backupID := runID + "-" + suffix
	registeredBundle, err := plan.Route.LoadManifest()
	if err != nil {
		return archive, nil, err
	}
	file, expected, err := client.PrepareBackup(ctx, workerclient.BackupSelection{
		ID: backupID, Reader: plan.Route.Reader, Captures: captures, Generations: plan.Generations, BundleManifest: registeredBundle,
	})
	if err != nil {
		return archive, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	err = errors.Join(err, file.Close())
	if err != nil || len(raw) > 16<<20 {
		return archive, nil, errors.Join(errors.New("invalid Worker backup manifest length"), err)
	}
	archive.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if archive.Digest != expected {
		return archive, nil, errors.New("Worker backup manifest digest differs from its receipt")
	}
	var manifest closureManifest
	if err := checkSelection(raw, backupID, plan, &manifest); err != nil {
		return archive, nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage, root), 0700); err != nil {
		return archive, nil, err
	}
	manifestPath := filepath.Join(root, "closure.json")
	if err := os.WriteFile(filepath.Join(stage, manifestPath), raw, 0600); err != nil {
		return archive, nil, err
	}
	files := []File{{Path: manifestPath, SHA256: strings.TrimPrefix(expected, "sha256:"), Bytes: int64(len(raw))}}
	for relative, bytes := range manifest.Members {
		if !validNativeMember(relative) || bytes < 0 {
			return archive, nil, errors.New("invalid Worker backup member")
		}
		file, _, err := client.OpenBackupMember(ctx, backupID, relative)
		if err != nil {
			return archive, nil, err
		}
		entry, err := copyNativeMember(ctx, stage, filepath.Join(root, relative), file, bytes)
		if err != nil {
			return archive, nil, err
		}
		files = append(files, entry)
	}
	bundle, err := copyNativeBundle(stage, root, plan.Route)
	return archive, append(files, bundle...), err
}

func checkSelection(raw []byte, id string, plan nativeSelection, manifest *closureManifest) error {
	if err := json.Unmarshal(raw, manifest); err != nil {
		return err
	}
	if manifest.Schema != "NativeBackupClosure/1" || manifest.ID != id || manifest.Bundle != plan.Route.Bundle || manifest.Reader != plan.Route.Reader ||
		len(manifest.Captures) != len(plan.Captures) || !slices.Equal(manifest.Generations, plan.Generations) {
		return errors.New("Worker backup scope differs from the catalog selection")
	}
	seen := make(map[string]bool)
	for _, capture := range manifest.Captures {
		if seen[capture.Point.ID] || plan.Captures[capture.Point.ID] != capture.Digest || capture.Digest == "" {
			return errors.New("Worker backup StatePoint differs from its committed receipt")
		}
		seen[capture.Point.ID] = true
	}
	return nil
}

func validNativeMember(path string) bool {
	return filepath.IsLocal(path) && filepath.Clean(path) == path && (strings.HasPrefix(path, "statepoints/") || strings.HasPrefix(path, "blocks/"))
}

func copyNativeMember(ctx context.Context, stage, relative string, source *os.File, expected int64) (result File, err error) {
	defer func() { err = errors.Join(err, source.Close()) }()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(stage, relative)), 0700); err != nil {
		return result, err
	}
	file, err := os.OpenFile(filepath.Join(stage, relative), os.O_WRONLY|os.O_EXCL|os.O_CREATE, 0600)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	bytes, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(backupReader{ctx, source}, expected+1))
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil || bytes != expected {
		return result, errors.Join(errors.New("native backup member length changed during export"), err)
	}
	return File{Path: relative, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), Bytes: bytes}, nil
}

type backupReader struct {
	ctx    context.Context
	source io.Reader
}

func (r backupReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buf)
}

func copyNativeBundle(stage, root string, route customsmolvm.Route) ([]File, error) {
	raw, err := route.LoadManifest()
	if err != nil {
		return nil, err
	}
	var spec installedBundle
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "bundle")
	dir := filepath.Join(stage, path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime-bundle.json"), raw, 0600); err != nil {
		return nil, err
	}
	files := []File{{Path: filepath.Join(path, "runtime-bundle.json"), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Bytes: int64(len(raw))}}
	for _, item := range spec.Artifacts {
		if !filepath.IsLocal(item.Path) || filepath.Clean(item.Path) != item.Path || item.Path == "." {
			return nil, errors.New("invalid runtime bundle member path")
		}
		relative := filepath.Join(path, item.Path)
		copied, err := digestFile(filepath.Dir(route.Manifest), item.Path, filepath.Join(stage, relative))
		if err != nil {
			return nil, err
		}
		if copied.SHA256 != item.SHA || copied.Bytes != item.Length {
			return nil, errors.New("installed runtime artifact differs from its pinned digest")
		}
		copied.Path = relative
		files = append(files, copied)
	}
	return files, nil
}

func verifyNative(ctx context.Context, db *sql.DB, cfg config.Config, archive string, backup Backup, verified map[string]File) error {
	plans, err := nativePlans(ctx, db, cfg)
	if err != nil {
		return err
	}
	if len(backup.Native) != len(plans) {
		return ErrNativeClosure
	}
	for i, native := range backup.Native {
		plan := plans[i]
		suffix := routeSuffix(plan.Route)
		if native.RuntimeInstance != plan.Route.InstanceID || native.Bundle != plan.Route.Bundle || native.Reader != plan.Route.Reader ||
			native.Directory != filepath.Join("native", suffix) {
			return ErrNativeClosure
		}
		name := filepath.Join(native.Directory, "closure.json")
		entry := verified[name]
		if entry.SHA256 == "" || "sha256:"+entry.SHA256 != native.Digest || entry.Bytes > 16<<20 {
			return ErrNativeClosure
		}
		file, err := openFile(archive, name)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		var manifest closureManifest
		if err := checkSelection(raw, backup.RunID+"-"+suffix, plan, &manifest); err != nil {
			return err
		}
		delete(verified, name)
		for relative, size := range manifest.Members {
			path := filepath.Join(native.Directory, relative)
			if !validNativeMember(relative) || verified[path].Path == "" || verified[path].Bytes != size {
				return ErrNativeClosure
			}
			delete(verified, path)
		}
		if err := verifyNativeBundle(archive, native, verified); err != nil {
			return err
		}
		if err := nativeReader(ctx, archive, native, plan.Route, "", false); err != nil {
			return err
		}
	}
	return nil
}

func verifyNativeBundle(archive string, native NativeArchive, verified map[string]File) error {
	base := filepath.Join(native.Directory, "bundle")
	name := filepath.Join(base, "runtime-bundle.json")
	if verified[name].Path == "" {
		return ErrNativeClosure
	}
	route := customsmolvm.Route{Bundle: native.Bundle, Reader: native.Reader, Manifest: filepath.Join(archive, name)}
	raw, err := route.LoadManifest()
	if err != nil {
		return err
	}
	var spec installedBundle
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	delete(verified, name)
	for _, item := range spec.Artifacts {
		if !filepath.IsLocal(item.Path) || filepath.Clean(item.Path) != item.Path || item.Path == "." {
			return ErrNativeClosure
		}
		path := filepath.Join(base, item.Path)
		asset := verified[path]
		if asset.Path == "" || asset.SHA256 != item.SHA || asset.Bytes != item.Length {
			return ErrNativeClosure
		}
		delete(verified, path)
	}
	return nil
}

func nativeReader(ctx context.Context, archive string, native NativeArchive, route customsmolvm.Route, output string, workerCatalog bool) error {
	raw, err := route.LoadManifest()
	if err != nil {
		return err
	}
	var spec installedBundle
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	worker := ""
	executableHash := ""
	for _, item := range spec.Artifacts {
		if item.Path == "smolvm-bin" {
			executableHash = item.SHA
		}
		if item.Path != "smolvm-resourced" {
			continue
		}
		file, err := digestFile(filepath.Dir(route.Manifest), item.Path, "")
		if err != nil || file.SHA256 != item.SHA || file.Bytes != item.Length {
			return ErrNativeClosure
		}
		worker = filepath.Join(filepath.Dir(route.Manifest), item.Path)
	}
	if worker == "" {
		return ErrNativeClosure
	}
	args := []string{"recover-backup-closure", "--archive", filepath.Join(archive, native.Directory), "--manifest-digest", native.Digest,
		"--bundle-digest", native.Bundle, "--state-reader-id", native.Reader}
	if output != "" {
		args = append(args, "--output", output)
	}
	if workerCatalog {
		if executableHash == "" {
			return ErrNativeClosure
		}
		args = append(args, "--bundle-manifest", route.Manifest, "--executable-sha256", executableHash)
	}
	if raw, err := exec.CommandContext(ctx, worker, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: installed reader rejected the closure: %s", ErrNativeClosure, strings.TrimSpace(string(raw)))
	}
	return nil
}

func ReleaseNativeBackup(ctx context.Context, cfg config.Config, runID string) error {
	for _, route := range cfg.Runtime.Custom {
		if route.WorkerSocket == "" {
			continue
		}
		client := workerclient.Client{Socket: route.WorkerSocket, Bundle: route.Bundle}
		if err := client.ReleaseBackup(ctx, runID+"-"+routeSuffix(route)); err != nil {
			var fault *workerclient.Fault
			if !errors.As(err, &fault) || fault.Code != "E_NOT_FOUND" && fault.Code != "E_BUNDLE_UNAVAILABLE" && fault.Code != "E_CAPABILITY_UNSUPPORTED" {
				return err
			}
		}
	}
	return nil
}

// RecoverNativeBackup verifies the authenticated archive and materializes each
// selected native closure in an independent directory before publishing it.
func RecoverNativeBackup(ctx context.Context, cfg config.Config, archive, keyFile, output string, workerCatalog bool) (Backup, error) {
	metadata, err := VerifyPlannedBackup(ctx, cfg, archive, keyFile)
	if err != nil {
		return metadata, err
	}
	if metadata.Scope != config.BackupNative || len(metadata.Native) == 0 || output == "" {
		return metadata, ErrNativeClosure
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return metadata, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".native-backup-recovery-")
	if err != nil {
		return metadata, err
	}
	defer os.RemoveAll(stage)
	for _, native := range metadata.Native {
		for _, route := range cfg.Runtime.Custom {
			if route.InstanceID == native.RuntimeInstance && route.Bundle == native.Bundle && route.Reader == native.Reader {
				if err := nativeReader(ctx, archive, native, route, filepath.Join(stage, filepath.Base(native.Directory)), workerCatalog); err != nil {
					return metadata, err
				}
				break
			}
		}
	}
	if err := syncTree(stage); err != nil {
		return metadata, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE); err != nil {
		return metadata, err
	}
	return metadata, syncPath(filepath.Dir(output))
}
