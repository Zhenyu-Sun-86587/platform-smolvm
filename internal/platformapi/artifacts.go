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

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/snapshots"
)

type blobRequest struct {
	Digest string `json:"sha256"`
	Size   int64  `json:"size_bytes"`
}

func (s *Server) artifactRoutes() {
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
		if err = os.Link(f.Name(), filepath.Join(dir, digest)); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return err
		}
		return s.accept(w, r, p, blobRequest{Digest: digest, Size: size}, registry.AcceptRequest{Kind: "snapshot_import", Heavy: true})
	})
	s.Extra["snapshot_delete"] = s.deleteSnapshot
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
			op, _, err := s.DB.Accept(ctx, registry.AcceptRequest{Kind: "snapshot_" + action, Request: lifecycle.JSON(map[string]any{"snapshot_id": snap.ID, "revision": rev, "verification": v.Verification}), Scope: env.Scope, Epoch: env.Epoch, Key: env.Key, PrincipalID: p.ID, CredentialID: p.CredentialID, Heavy: action == "verify"})
			if err != nil {
				return err
			}
			w.Header().Set("Location", "/v1/operations/"+op.ID)
			return write(w, 202, op)
		})
	}
}
func (s *Server) importSnapshot(ctx context.Context, j *lifecycle.Job) error {
	var v blobRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	cat := s.Snapshots
	id := "snap_" + j.Op.ID[3:]
	file := filepath.Join(cat.Stage, "uploads", v.Digest)
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
		return err
	}
	if err = cat.Publish(ctx, id, a, false); err != nil {
		return err
	}
	j.Written = true
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": id})})
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
	res, err := s.DB.DB().ExecContext(ctx, `UPDATE snapshots SET state='deleting',revision=revision+1 WHERE id=? AND revision=? AND state IN ('ready','quarantined') AND NOT EXISTS (SELECT 1 FROM snapshot_pins WHERE snapshot_id=?)`, v.ID, v.Revision, v.ID)
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
	if snap.Revision != v.Revision {
		return registry.ErrRevision
	}
	if v.Verification != "checksum" {
		return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "isolated restore verification requires an independent test runtime"}
	}
	if err = s.Snapshots.Pin(ctx, v.ID, "verify", j.Op.ID); err != nil {
		return err
	}
	defer s.Snapshots.Unpin(context.Background(), v.ID, "verify", j.Op.ID)
	if err = s.Snapshots.Verify(ctx, snap); err != nil {
		return err
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": v.ID, "verification": "checksum"})})
}
