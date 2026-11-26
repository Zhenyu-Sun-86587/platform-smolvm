package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/nodes"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
	"sandbox-platform/internal/runtime/workerclient"
)

type NativeArchive struct {
	WorkerID        string `json:"worker_id,omitempty"`
	LocalInstanceID string `json:"local_instance_id,omitempty"`
	RuntimeInstance string `json:"runtime_instance_id"`
	Bundle          string `json:"bundle_digest"`
	Reader          string `json:"state_reader_id"`
	Directory       string `json:"directory"`
	Digest          string `json:"manifest_digest"`
}

type nativeSelection struct {
	Route           customsmolvm.Route
	Node            *config.Node
	LocalInstanceID string
	Captures        map[string]string
	Generations     []string
}

type pinnedNative struct {
	client      workerclient.BackupControl
	closeClient func()
	archive     NativeArchive
	route       customsmolvm.Route
	id          string
	raw         []byte
	manifest    closureManifest
}

var ErrNativeBackupRetry = errors.New("native backup is awaiting Worker reconciliation")

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
	return nodeproto.BackupSuffix(route.InstanceID, route.Bundle)
}

func backupRoutes(cfg config.Config) []nativeSelection {
	routes := make([]nativeSelection, 0, len(cfg.Runtime.Custom))
	for _, r := range cfg.Runtime.Custom {
		routes = append(routes, nativeSelection{Route: r})
	}
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		for _, r := range n.Routes {
			installed := customsmolvm.Route{InstanceID: r.RuntimeInstanceID, Bundle: r.BundleDigest, Reader: r.Reader, Manifest: r.Manifest}
			routes = append(routes, nativeSelection{Route: installed, Node: n, LocalInstanceID: r.LocalInstanceID})
		}
	}
	return routes
}

func (p nativeSelection) backupClient(ctx context.Context, store *registry.Store) (workerclient.BackupControl, func(), error) {
	if p.Node == nil {
		return workerclient.Client{Socket: p.Route.WorkerSocket, Bundle: p.Route.Bundle}, func() {}, nil
	}
	control, err := store.Control(ctx)
	if err != nil {
		return nil, nil, err
	}
	c, _, err := nodes.NewClient(*p.Node, control, store)
	if err != nil {
		return nil, nil, err
	}
	a := c.Adapter(nodeproto.Route{RuntimeInstanceID: p.Route.InstanceID, LocalInstanceID: p.LocalInstanceID, BundleDigest: p.Route.Bundle})
	return a, c.CloseIdleConnections, nil
}

func nativePlans(ctx context.Context, db *sql.DB, cfg config.Config) ([]nativeSelection, error) {
	routes := make(map[string]nativeSelection)
	for _, r := range backupRoutes(cfg) {
		routes[r.Route.InstanceID] = r
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
		candidate, ok := routes[routeID]
		route := candidate.Route
		if !ok || route.Bundle != bundle || route.WorkerSocket == "" && candidate.Node == nil {
			return nil, fmt.Errorf("%w: endpoint for the pinned bundle is unavailable", ErrNativeClosure)
		}
		if selected[routeID] == nil {
			candidate.Captures = map[string]string{}
			candidate.Generations = []string{}
			if candidate.Node != nil {
				var owner, local string
				err := db.QueryRowContext(ctx, "SELECT worker_id,local_instance_id FROM runtime_instances WHERE id=? AND runtime_bundle_id=?", routeID, bundle).Scan(&owner, &local)
				if err != nil || owner != candidate.Node.ID || local != candidate.LocalInstanceID {
					return nil, ErrNativeClosure
				}
			}
			selected[routeID] = &candidate
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

func nativeEndpoints(plans []nativeSelection) []registry.NativeBackupRoute {
	routes := make([]registry.NativeBackupRoute, 0, len(plans))
	for _, plan := range plans {
		route := plan.Route
		endpoint := registry.NativeBackupRoute{Instance: route.InstanceID, Bundle: route.Bundle, Reader: route.Reader, Socket: route.WorkerSocket}
		if plan.Node != nil {
			endpoint.WorkerID = plan.Node.ID
			endpoint.LocalInstanceID = plan.LocalInstanceID
		}
		routes = append(routes, endpoint)
	}
	return routes
}

func sealNativeCatalog(ctx context.Context, cfg config.Config, plan *BackupPlan, stage string, copied bool,
	copyCatalog, seal func() error) error {
	stored, exists, err := plan.Registry.NativeBackupSelection(ctx, plan.ID)
	if err != nil {
		return err
	}
	if exists {
		if !copied {
			return fmt.Errorf("%w: sealed catalog is missing", ErrNativeClosure)
		}
		file, err := digestFile(stage, databaseName, "")
		if err != nil || file.SHA256 != stored.CatalogSHA256 {
			return errors.Join(ErrNativeClosure, errors.New("sealed catalog digest changed"), err)
		}
		return nil
	}
	run, err := plan.Registry.GetBackup(ctx, plan.ID)
	if err != nil {
		return err
	}
	err = plan.Registry.PinNativeBackupSelection(ctx, run, func() (registry.NativeBackupSelection, error) {
		var result registry.NativeBackupSelection
		// A previous platform can have pinned one or more Worker routes. Adopt
		// its durable catalog without selecting resources again.
		if !copied {
			if err := copyCatalog(); err != nil {
				return result, err
			}
		}
		file, err := digestFile(stage, databaseName, "")
		if err != nil {
			return result, err
		}
		result.CatalogSHA256 = file.SHA256
		db, err := openStandaloneDB(filepath.Join(stage, databaseName))
		if err != nil {
			return result, err
		}
		defer db.Close()
		var allowed bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_runs r JOIN meta m ON m.id=1
			WHERE r.id=? AND r.scope=? AND r.state='running' AND r.control_epoch=? AND m.control_epoch=r.control_epoch)`,
			run.ID, config.BackupNative, run.ControlEpoch).Scan(&allowed); err != nil {
			return result, err
		}
		if !allowed {
			return result, fmt.Errorf("%w: staged catalog belongs to a different backup run", ErrNativeClosure)
		}
		plans, err := nativePlans(ctx, db, cfg)
		if err != nil {
			return result, err
		}
		result.Routes = nativeEndpoints(plans)
		result.Snapshots, err = selectBackupIDs(ctx, db, `SELECT id FROM snapshots WHERE state='ready' ORDER BY id`)
		if err != nil {
			return result, err
		}
		result.VolumeSnapshots, err = selectBackupIDs(ctx, db, `SELECT id FROM volume_snapshots WHERE state='available' ORDER BY id`)
		if err != nil {
			return result, err
		}
		return result, err
	}, func() error {
		if copied {
			return nil
		}
		return seal()
	})
	if errors.Is(err, registry.ErrNativeBackupBusy) {
		return errors.Join(ErrNativeBackupRetry, err)
	}
	return err
}

func selectBackupIDs(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func nativeWorkerError(err error) error {
	var worker *workerclient.Fault
	var transport *net.OpError
	if errors.Is(err, rt.ErrUnavailable) || errors.As(err, &transport) || errors.Is(err, io.EOF) || errors.Is(err, unix.ECONNRESET) ||
		(errors.As(err, &worker) && worker.Code == "E_RECOVERING") {
		return errors.Join(ErrNativeBackupRetry, err)
	}
	return errors.Join(ErrNativeClosure, err)
}

func pinNative(ctx context.Context, db *sql.DB, cfg config.Config, backup *BackupPlan) ([]pinnedNative, error) {
	plans, err := nativePlans(ctx, db, cfg)
	if err != nil {
		return nil, err
	}
	stored, exists, err := backup.Registry.NativeBackupSelection(ctx, backup.ID)
	if err != nil {
		return nil, err
	}
	if !exists || !slices.Equal(stored.Routes, nativeEndpoints(plans)) {
		return nil, fmt.Errorf("%w: installed endpoints differ from the sealed selection", ErrNativeClosure)
	}
	var pinned []pinnedNative
	for _, plan := range plans {
		closure, err := pinNativeRoute(ctx, backup.Registry, backup.ID, plan)
		if err != nil {
			return nil, err
		}
		pinned = append(pinned, closure)
	}
	if err := backup.Registry.ConfirmNativeBackupPins(ctx, backup.ID, stored.CatalogSHA256); err != nil {
		// Worker pins may already be durable. Repeat confirmation under the
		// same catalog and route identities on the next scheduler pass.
		return nil, errors.Join(ErrNativeBackupRetry, err)
	}
	return pinned, nil
}

func exportNative(ctx context.Context, stage string, pinned []pinnedNative) ([]NativeArchive, []File, error) {
	var archives []NativeArchive
	var files []File
	for _, plan := range pinned {
		members, err := exportNativeRoute(ctx, stage, plan)
		if err != nil {
			return nil, nil, err
		}
		archive := plan.archive
		archives = append(archives, archive)
		files = append(files, members...)
	}
	return archives, files, nil
}

func pinNativeRoute(ctx context.Context, store *registry.Store, runID string, plan nativeSelection) (pinnedNative, error) {
	suffix := routeSuffix(plan.Route)
	root := filepath.Join("native", suffix)
	pinned := pinnedNative{id: runID + "-" + suffix, route: plan.Route,
		archive: NativeArchive{RuntimeInstance: plan.Route.InstanceID, Bundle: plan.Route.Bundle, Reader: plan.Route.Reader, Directory: root}}
	captures := make([]string, 0, len(plan.Captures))
	for id := range plan.Captures {
		captures = append(captures, id)
	}
	slices.Sort(captures)
	client, closeClient, err := plan.backupClient(ctx, store)
	if err != nil {
		return pinned, err
	}
	defer closeClient()
	pinned.client, pinned.closeClient = client, closeClient
	if plan.Node != nil {
		pinned.archive.WorkerID = plan.Node.ID
		pinned.archive.LocalInstanceID = plan.LocalInstanceID
	}
	registeredBundle, err := plan.Route.LoadManifest()
	if err != nil {
		return pinned, errors.Join(ErrNativeClosure, err)
	}
	file, expected, err := client.BackupManifest(ctx, workerclient.BackupSelection{
		ID: pinned.id, Reader: plan.Route.Reader, Captures: captures, Generations: plan.Generations, BundleManifest: registeredBundle,
	})
	if err != nil {
		return pinned, nativeWorkerError(err)
	}
	raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	err = errors.Join(err, file.Close())
	if err != nil || len(raw) > 16<<20 {
		return pinned, errors.Join(ErrNativeClosure, errors.New("invalid Worker backup manifest length"), err)
	}
	pinned.archive.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if pinned.archive.Digest != expected {
		return pinned, fmt.Errorf("%w: Worker backup manifest digest differs from its receipt", ErrNativeClosure)
	}
	if err := checkSelection(raw, pinned.id, plan, &pinned.manifest); err != nil {
		return pinned, errors.Join(ErrNativeClosure, err)
	}
	pinned.raw = raw
	return pinned, nil
}

func exportNativeRoute(ctx context.Context, stage string, pinned pinnedNative) ([]File, error) {
	root := pinned.archive.Directory
	if err := os.MkdirAll(filepath.Join(stage, root), 0700); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(root, "closure.json")
	if err := os.WriteFile(filepath.Join(stage, manifestPath), pinned.raw, 0600); err != nil {
		return nil, err
	}
	files := []File{{Path: manifestPath, SHA256: strings.TrimPrefix(pinned.archive.Digest, "sha256:"), Bytes: int64(len(pinned.raw))}}
	client := pinned.client
	defer pinned.closeClient()
	for relative, bytes := range pinned.manifest.Members {
		if !validNativeMember(relative) || bytes < 0 {
			return nil, fmt.Errorf("%w: invalid Worker backup member", ErrNativeClosure)
		}
		file, size, err := client.BackupMember(ctx, pinned.id, relative)
		if err != nil {
			return nil, nativeWorkerError(err)
		}
		if size != bytes {
			file.Close()
			return nil, errors.Join(ErrNativeClosure, errors.New("backup member length differs from manifest"))
		}
		entry, err := copyNativeMember(ctx, stage, filepath.Join(root, relative), file, bytes)
		if err != nil {
			return nil, errors.Join(ErrNativeClosure, err)
		}
		files = append(files, entry)
	}
	bundle, err := copyNativeBundle(stage, root, pinned.route)
	if err != nil {
		return nil, errors.Join(ErrNativeClosure, err)
	}
	return append(files, bundle...), nil
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

func copyNativeMember(ctx context.Context, stage, relative string, source io.ReadCloser, expected int64) (result File, err error) {
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
		if native.WorkerID != "" && (plan.Node == nil || plan.Node.ID != native.WorkerID || plan.LocalInstanceID != native.LocalInstanceID) || native.WorkerID == "" && plan.Node != nil {
			return ErrNativeClosure
		}
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

func ReleaseNativeBackup(ctx context.Context, cfg config.Config, store *registry.Store, runID string) error {
	stored, exists, err := store.NativeBackupSelection(ctx, runID)
	if err != nil {
		return err
	}
	routes := stored.Routes
	if !exists {
		// An installation may be resuming a run accepted by an older platform.
		for _, route := range cfg.Runtime.Custom {
			if route.WorkerSocket != "" {
				routes = append(routes, registry.NativeBackupRoute{Instance: route.InstanceID, Bundle: route.Bundle,
					Reader: route.Reader, Socket: route.WorkerSocket})
			}
		}
	}
	for _, route := range routes {
		var client workerclient.BackupControl = workerclient.Client{Socket: route.Socket, Bundle: route.Bundle}
		closeClient := func() {}
		if route.WorkerID != "" {
			found := false
			for _, r := range backupRoutes(cfg) {
				if r.Node == nil || r.Node.ID != route.WorkerID || r.LocalInstanceID != route.LocalInstanceID || r.Route.InstanceID != route.Instance || r.Route.Bundle != route.Bundle || r.Route.Reader != route.Reader {
					continue
				}
				client, closeClient, err = r.backupClient(ctx, store)
				if err != nil {
					return err
				}
				found = true
				break
			}
			if !found {
				return ErrNativeClosure
			}
		}
		suffix := routeSuffix(customsmolvm.Route{InstanceID: route.Instance, Bundle: route.Bundle})
		err := client.ReleaseBackup(ctx, runID+"-"+suffix)
		closeClient()
		if err != nil {
			var fault *workerclient.Fault
			if !errors.As(err, &fault) {
				return err
			}
			switch fault.Code {
			case "E_NOT_FOUND":
				continue
			case "E_BUNDLE_UNAVAILABLE", "E_CAPABILITY_UNSUPPORTED":
				if !exists {
					continue
				}
			}
			return err
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
		found := false
		for _, r := range backupRoutes(cfg) {
			route := r.Route
			if route.InstanceID == native.RuntimeInstance && route.Bundle == native.Bundle && route.Reader == native.Reader {
				if err := nativeReader(ctx, archive, native, route, filepath.Join(stage, filepath.Base(native.Directory)), workerCatalog); err != nil {
					return metadata, err
				}
				found = true
				break
			}
		}
		if !found {
			return metadata, ErrNativeClosure
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
