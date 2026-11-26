package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

type blobRequest struct {
	Digest   string `json:"sha256"`
	Size     int64  `json:"size_bytes"`
	StageKey string `json:"stage_key,omitempty"`
}

func (s *Server) artifactRoutes() {
	s.pruneCompletedUploads()
	s.Extra["snapshot_import"] = s.importSnapshot
	s.route("POST /v1/snapshots/import", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("manage", ""); err != nil {
			return err
		}
		ctl, err := s.DB.Control(r.Context())
		if err != nil {
			return err
		}
		if r.Header.Get("If-Control-Epoch") != ctl.ControlEpoch {
			return registry.ErrControlEpoch
		}
		if ctl.Maintenance {
			return registry.ErrMaintenance
		}
		if r.Header.Get("Idempotency-Key") == "" {
			return registry.ErrInvalid
		}
		var fs syscall.Statfs_t
		if err = syscall.Statfs(s.Snapshots.Stage, &fs); err != nil {
			return err
		}
		available := fs.Bavail * uint64(fs.Bsize)
		reserve := max(s.Config.Storage.MinBytes, fs.Blocks*uint64(fs.Bsize)/100*uint64(s.Config.Storage.MinPercent))
		size := r.ContentLength
		if size < 1 || size > 1<<40 || available <= reserve || uint64(size) > (available-reserve)/2 {
			return &lifecycle.Failure{Code: "DISK_LOW", Message: "insufficient space or invalid Content-Length for staged import"}
		}
		digest := r.Header.Get("X-Content-SHA256")
		b, err := hex.DecodeString(digest)
		if err != nil || len(b) != 32 || strings.ToLower(digest) != digest {
			return registry.ErrInvalid
		}
		dir := filepath.Join(s.Snapshots.Stage, "uploads")
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		f, err := os.CreateTemp(dir, ".upload-")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, size+1))
		if err != nil {
			return err
		}
		if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
			return fmt.Errorf("%w: upload checksum or length mismatch", registry.ErrInvalid)
		}
		if err = f.Sync(); err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		identity := sha256.Sum256([]byte(strings.Join([]string{p.ID, ctl.ControlEpoch, r.Header.Get("Idempotency-Key"), digest}, "\x00")))
		stageKey := hex.EncodeToString(identity[:])
		stagePath := filepath.Join(dir, stageKey)
		linkErr := os.Link(f.Name(), stagePath)
		if linkErr != nil && !errors.Is(linkErr, os.ErrExist) {
			return linkErr
		}
		env := envelope(r, p, blobRequest{Digest: digest, Size: size, StageKey: stageKey})
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return err
		}
		op, _, err := s.DB.Accept(r.Context(), registry.AcceptRequest{Kind: "snapshot_import", Heavy: true, PrincipalID: p.ID,
			CredentialID: p.CredentialID, Epoch: env.Epoch, Scope: env.Scope, Key: env.Key, Request: env.Request})
		if err != nil {
			// An accepting connection may disappear after its commit. Keep
			// staging until a proven rejection or the operation completes.
			if linkErr == nil && (errors.Is(err, registry.ErrIdempotency) || errors.Is(err, registry.ErrInvalid) ||
				errors.Is(err, registry.ErrControlEpoch) || errors.Is(err, registry.ErrMaintenance)) {
				_ = os.Remove(stagePath)
			}
			return err
		}
		if op.Terminal() {
			_ = os.Remove(stagePath)
		}
		w.Header().Set("Location", "/v1/operations/"+op.ID)
		return write(w, 202, op)
	})
	s.Extra["snapshot_delete"] = s.deleteSnapshot
	s.Extra[rt.VMSnapshotDelete], s.Recover[rt.VMSnapshotDelete] = s.deleteVMSnapshot, s.deleteVMSnapshot
	s.Extra["snapshot_verify"] = s.verifySnapshot
	for _, action := range []string{"verify", "delete"} {
		path := "POST /v1/snapshots/{snap}/verify"
		if action == "delete" {
			path = "DELETE /v1/snapshots/{snap}"
		}
		s.route(path, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			ctx := r.Context()
			snap, err := s.Snapshots.Get(ctx, r.PathValue("snap"))
			if err != nil {
				return err
			}
			if err = p.Authorize("manage", snap.MachineID); err != nil {
				return err
			}
			kind := "snapshot_" + action
			job := map[string]any{"snapshot_id": snap.ID}
			var admitRuntime func(rt.BundleRoute, string) error
			if snap.VM != nil {
				if action != "delete" {
					return rt.ErrUnsupported
				}
				admitRuntime = func(rt.BundleRoute, string) error {
					_, err := s.vmSnapshotWorker(snap)
					return err
				}
				kind = rt.VMSnapshotDelete
				job["runtime_instance_id"] = snap.VM.RuntimeInstanceID
				job["runtime_bundle_id"] = snap.VM.RuntimeBundleID
			}
			rev, err := revision(r)
			if err != nil {
				return err
			}
			var v struct {
				Verification string `json:"verification,omitempty"`
			}
			if err = decode(r, &v); err != nil {
				return err
			}
			if action == "verify" && v.Verification != "checksum" && v.Verification != "restore" {
				return registry.ErrInvalid
			}
			env := envelope(r, p, v)
			job["revision"], job["verification"] = rev, v.Verification
			op, _, err := s.DB.Accept(ctx, registry.AcceptRequest{Kind: kind, Request: lifecycle.JSON(job), Scope: env.Scope, Epoch: env.Epoch, Key: env.Key, PrincipalID: p.ID, CredentialID: p.CredentialID, Heavy: action == "verify", AdmitRuntime: admitRuntime})
			if err != nil {
				return err
			}
			w.Header().Set("Location", "/v1/operations/"+op.ID)
			return write(w, 202, op)
		})
	}
}

func (s *Server) pruneCompletedUploads() {
	dir := filepath.Join(s.Snapshots.Stage, "uploads")
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, entry := range files {
		key := entry.Name()
		if raw, err := hex.DecodeString(key); err != nil || len(raw) != 32 || !entry.Type().IsRegular() {
			continue
		}
		var terminal bool
		err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM operations WHERE kind='snapshot_import' AND json_extract(request_json,'$.stage_key')=?
			AND state IN ('succeeded','failed','cancelled'))`, key).Scan(&terminal)
		if err == nil && terminal {
			_ = os.Remove(filepath.Join(dir, key))
		}
	}
}
func (s *Server) importSnapshot(ctx context.Context, j *lifecycle.Job) error {
	var v blobRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	cat := s.Snapshots
	id := "snap_" + j.Op.ID[3:]
	key := v.StageKey
	if key == "" {
		key = v.Digest
	}
	if raw, err := hex.DecodeString(key); err != nil || len(raw) != 32 || key != strings.ToLower(key) {
		return registry.ErrInvalid
	}
	file := filepath.Join(cat.Stage, "uploads", key)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = j.Stage(ctx, "static_import", ""); err != nil {
		return err
	}
	dir, err := cat.Reserve(ctx, id, "", "unknown", "", 0)
	if err != nil {
		return err
	}
	a, err := snapshots.Import(dir, f, v.Size, cat.Trust)
	if err != nil {
		if e := cat.Quarantine(ctx, id); e != nil {
			return e
		}
		if v.StageKey != "" {
			_ = os.Remove(file)
		}
		return err
	}
	if err = cat.Publish(ctx, id, a, false); err != nil {
		return err
	}
	j.Written = true
	if err := j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": id})}); err != nil {
		return err
	}
	if v.StageKey != "" {
		_ = os.Remove(file)
	}
	return nil
}
func (s *Server) deleteSnapshot(ctx context.Context, j *lifecycle.Job) error {
	var v struct {
		ID       string `json:"snapshot_id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	if err := j.Stage(ctx, "delete_artifact", ""); err != nil {
		return err
	}
	res, err := s.DB.DB().ExecContext(ctx, `UPDATE snapshots SET state='deleting',revision=revision+1 WHERE id=? AND revision=? AND state IN ('ready','quarantined')
		AND NOT EXISTS (SELECT 1 FROM vm_snapshot_provenance WHERE snapshot_id=snapshots.id)
		AND NOT EXISTS (SELECT 1 FROM snapshot_pins WHERE snapshot_id=?)
		AND NOT EXISTS (SELECT 1 FROM operations WHERE kind IN ('restore','rollback','snapshot_verify')
			AND state NOT IN ('succeeded','failed','cancelled') AND json_extract(request_json,'$.snapshot_id')=?)`,
		v.ID, v.Revision, v.ID, v.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return &lifecycle.Failure{Code: "DEPENDENCY_BLOCKED", Message: "snapshot pinned, unavailable or revision changed"}
	}
	j.Written = true
	for _, root := range []string{s.Snapshots.Root, s.Snapshots.Stage} {
		if err = os.RemoveAll(filepath.Join(root, v.ID)); err != nil {
			return err
		}
		f, err := os.Open(root)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": v.ID}), Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE snapshots SET state='deleted',revision=revision+1 WHERE id=? AND state='deleting'", v.ID)
		return err
	}})
}
func (s *Server) verifySnapshot(ctx context.Context, j *lifecycle.Job) error {
	var v struct {
		ID           string `json:"snapshot_id"`
		Revision     int64  `json:"revision"`
		Verification string `json:"verification"`
	}
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	snap, err := s.Snapshots.Get(ctx, v.ID)
	if err != nil {
		return err
	}
	if snap.VM != nil {
		return rt.ErrUnsupported
	}
	if snap.Revision != v.Revision {
		return registry.ErrRevision
	}
	if v.Verification == "restore" {
		err = s.Snapshots.Pin(ctx, v.ID, "verify", j.Op.ID)
	} else {
		err = s.Snapshots.PinChecksum(ctx, v.ID, j.Op.ID)
	}
	if err != nil {
		return err
	}
	retain := false
	defer func() {
		if !retain {
			_ = s.Snapshots.Unpin(context.WithoutCancel(ctx), v.ID, "verify", j.Op.ID)
		}
	}()
	if err = s.Snapshots.Verify(ctx, snap); err != nil {
		return err
	}
	if v.Verification == "restore" {
		err = s.verifySnapshotRestore(ctx, j, snap)
		retain = err != nil && (j.Written || errors.Is(err, rt.ErrUncertain))
		return err
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": v.ID, "verification": "checksum"})})
}
