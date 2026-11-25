package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
	"sandbox-platform/internal/tasks"
	"time"
)

type copyRequest struct {
	SnapshotID         string `json:"snapshot_id,omitempty"`
	Label              string `json:"label,omitempty"`
	Purpose            string `json:"purpose,omitempty"`
	ExpectedGeneration int64  `json:"expected_generation,omitempty"`
	CopyPolicy         string `json:"copy_policy,omitempty"`
	ChildName          string `json:"child_name,omitempty"`
	Name               string `json:"name,omitempty"`
	Disposition        string `json:"task_disposition,omitempty"`
	OldStatePolicy     string `json:"old_state_policy,omitempty"`
}

func (s *Server) snapshotRoutes() {
	for _, k := range []string{"snapshot", "clone", "restore", "rollback"} {
		s.Extra[k] = s.copyJob
	}
	for route, kind := range map[string]string{"POST /v1/machines/{machine}/snapshots": "snapshot", "POST /v1/machines/{machine}/clones": "clone", "POST /v1/machines/{machine}/rollback": "rollback", "POST /v1/restores": "restore"} {
		s.route(route, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			mid := r.PathValue("machine")
			if err := p.Authorize("manage", mid); err != nil {
				return err
			}
			if (kind == "clone" || kind == "restore") && p.Scope.MachineID != "" {
				return auth.ErrForbidden
			}
			var v copyRequest
			if err := decode(r, &v); err != nil {
				return err
			}
			req := registry.AcceptRequest{Kind: kind, MachineID: mid, Heavy: true, ExpectedGeneration: v.ExpectedGeneration}
			switch kind {
			case "snapshot":
				if v.Purpose != "recovery" && v.Purpose != "clone" {
					return registry.ErrInvalid
				}
			case "clone":
				if v.CopyPolicy != "" && v.CopyPolicy != "managed_clean" {
					return registry.ErrInvalid
				}
				req.Name = v.ChildName
			case "rollback", "restore":
				item, err := s.Snapshots.Get(r.Context(), v.SnapshotID)
				if err != nil {
					return err
				}
				if item.State != "ready" || item.Content == "unknown" {
					return registry.ErrConflict
				}
				var m snapshots.Manifest
				if err = json.Unmarshal(item.Manifest, &m); err != nil {
					return err
				}
				if kind == "restore" {
					if item.Content != "clean" {
						return registry.ErrConflict
					}
					req.Name = v.Name
					req.TemplateID = m.TemplateID
				} else {
					if v.Disposition != "" && v.Disposition != "resume_captured" && v.Disposition != "cancel_captured" {
						return registry.ErrInvalid
					}
					if v.OldStatePolicy != "" && v.OldStatePolicy != "export_then_delete_on_paused" {
						return registry.ErrInvalid
					}
					if m.SourceMachineID != mid {
						return registry.ErrConflict
					}
					req.AllocateBinding = true
				}
			}
			return s.accept(w, r, p, v, req)
		})
	}
	s.route("GET /v1/machines/{machine}/snapshots", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		out, err := s.Snapshots.List(r.Context(), id)
		if err != nil {
			return err
		}
		return list(w, r, out, func(s snapshots.Snapshot) string { return s.ID })
	})
	s.route("GET /v1/snapshots/{snap}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		item, err := s.Snapshots.Get(r.Context(), r.PathValue("snap"))
		if err != nil {
			return err
		}
		if err = p.Authorize("read", item.MachineID); err != nil {
			return err
		}
		return write(w, 200, item)
	})
	s.route("GET /v1/snapshots/{snap}/export", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		ctx := r.Context()
		item, err := s.Snapshots.Get(ctx, r.PathValue("snap"))
		if err != nil {
			return err
		}
		if err = p.Authorize("manage", item.MachineID); err != nil {
			return err
		}
		owner, err := registry.NewID("export_")
		if err != nil {
			return err
		}
		if err = s.Snapshots.Pin(ctx, item.ID, "export", owner); err != nil {
			return err
		}
		defer s.Snapshots.Unpin(context.Background(), item.ID, "export", owner)
		if err = s.Snapshots.Verify(ctx, item); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", item.ID+".tar"))
		return snapshots.Export(w, filepath.Join(s.Snapshots.Root, item.ID))
	})
}
func (s *Server) copyJob(ctx context.Context, j *lifecycle.Job) error {
	var v copyRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	var source *registry.Binding
	if j.Op.Kind == "snapshot" || j.Op.Kind == "rollback" || j.Op.Kind == "clone" {
		id := j.Op.MachineID
		if j.Op.Kind == "clone" {
			id = j.Op.SourceMachineID
		}
		m, err := s.DB.GetMachine(ctx, id)
		if err != nil {
			return err
		}
		source = m.CurrentBinding
		if source == nil {
			return registry.ErrGeneration
		}
	}
	if j.Op.Kind == "snapshot" || j.Op.Kind == "clone" {
		purpose := v.Purpose
		if j.Op.Kind == "clone" {
			purpose = "clone"
		}
		id, err := s.capture(ctx, j, *source, purpose, v.Label, false)
		if err != nil {
			return err
		}
		v.SnapshotID = id
		if j.Op.Kind == "snapshot" {
			return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"snapshot_id": id})})
		}
	}
	return s.restore(ctx, j, source, v)
}
func (s *Server) capture(ctx context.Context, j *lifecycle.Job, b registry.Binding, purpose, label string, paused bool) (string, error) {
	m, err := s.DB.GetMachine(ctx, b.MachineID)
	if err != nil {
		return "", err
	}
	tpl, err := s.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil {
		return "", err
	}
	observed, err := s.Runtime.Observe(ctx, b.RuntimeName)
	if err != nil {
		return "", err
	}
	if observed.Labels["platform.binding"] != b.ID {
		return "", registry.ErrStaleObservation
	}
	mode := "prepare_copy"
	if purpose == "recovery" {
		mode = "prepare_recovery_capture"
	}
	content, guest := "unknown", "unknown"
	var evidence json.RawMessage
	if !paused {
		if observed.State != "running" {
			return "", registry.ErrConflict
		}
		if err = j.Stage(ctx, "prepare_capture", ""); err != nil {
			return "", err
		}
		res, e := j.Call(ctx, b, tasks.Request{Method: mode, OperationID: j.Op.ID})
		if e != nil {
			return "", e
		}
		j.Written = true
		if res.Capture == nil || !res.Capture.Ready || res.Capture.Source != *lifecycle.GuestBinding(b) {
			return "", errors.New("invalid guest capture receipt")
		}
		content = res.Capture.Content
		guest = "ready"
		evidence = lifecycle.JSON(res.Capture)
	}
	id, err := registry.NewID("snap_")
	if err != nil {
		return "", err
	}
	dir, err := s.Snapshots.Reserve(ctx, id, b.MachineID, purpose, label, b.Generation)
	if err != nil {
		return "", err
	}
	if err = j.Stage(ctx, "checkpoint", ""); err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	go func() {
		var err error
		if paused {
			_, err = s.Runtime.ExportPausedCheckpoint(ctx, b.RuntimeName, pw)
		} else {
			_, err = s.Runtime.CaptureCheckpoint(ctx, b.RuntimeName, pw)
		}
		pw.CloseWithError(err)
	}()
	manifest := snapshots.Manifest{TemplateID: m.TemplateID, TemplateDigest: tpl.ImageDigest, SourceMachineID: m.ID, SourceGeneration: b.Generation, OperationID: j.Op.ID, Purpose: purpose, TaskContent: content, GuestStatus: guest, CaptureReceipt: evidence, Dependencies: []string{}}
	artifact, err := snapshots.Capture(dir, pr, 1<<40, manifest, s.Snapshots.KeyID, s.Snapshots.Key)
	pr.CloseWithError(err)
	if err != nil {
		_ = s.Snapshots.Quarantine(ctx, id)
		return "", err
	}
	j.Written = true
	if err = s.Snapshots.Publish(ctx, id, artifact, true); err != nil {
		return "", err
	}
	if !paused {
		if _, err = j.Call(ctx, b, tasks.Request{Method: "finish_capture", OperationID: j.Op.ID}); err != nil {
			return "", err
		}
	}
	return id, nil
}
func (s *Server) restore(ctx context.Context, j *lifecycle.Job, old *registry.Binding, v copyRequest) error {
	cat := s.Snapshots
	item, err := cat.Get(ctx, v.SnapshotID)
	if err != nil {
		return err
	}
	if item.State != "ready" || item.Content == "unknown" || (j.Op.Kind != "rollback" && item.Content != "clean") {
		return registry.ErrConflict
	}
	if err = cat.Pin(ctx, item.ID, "operation", j.Op.ID); err != nil {
		return err
	}
	// An ambiguous import can retain a native dependency, so preserve its pin.
	if err = cat.Verify(ctx, item); err != nil {
		return err
	}
	var manifest snapshots.Manifest
	if err = json.Unmarshal(item.Manifest, &manifest); err != nil {
		return err
	}
	m, err := s.DB.GetMachine(ctx, j.Op.MachineID)
	if err != nil {
		return err
	}
	tpl, err := s.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil {
		return err
	}
	if tpl.ImageDigest != manifest.TemplateDigest {
		return errors.New("template digest mismatch")
	}
	var capture tasks.Capture
	if err = json.Unmarshal(manifest.CaptureReceipt, &capture); err != nil || !capture.Ready || capture.Source.MachineID != manifest.SourceMachineID || capture.Source.Generation != manifest.SourceGeneration || capture.Content != item.Content {
		return errors.New("capture receipt differs from signed metadata")
	}
	fence := ""
	if j.Op.Kind == "rollback" {
		if old == nil || capture.Source.MachineID != j.Op.MachineID {
			return registry.ErrGeneration
		}
		o, err := s.Runtime.Observe(ctx, old.RuntimeName)
		if err != nil {
			return err
		}
		if o.Labels["platform.binding"] != old.ID {
			return registry.ErrStaleObservation
		}
		switch o.State {
		case "running":
			if err = j.Stage(ctx, "stop_old", ""); err != nil {
				return err
			}
			if _, err = s.Runtime.Stop(ctx, old.RuntimeName); err != nil {
				return err
			}
			j.Written = true
		case "paused":
			if v.OldStatePolicy != "export_then_delete_on_paused" {
				return &lifecycle.Failure{Code: "PRECONDITION_FAILED", Message: "paused rollback requires export_then_delete_on_paused"}
			}
			id, err := s.capture(ctx, j, *old, "recovery", "pre-replacement saved execution", true)
			if err != nil {
				return err
			}
			if err = cat.Pin(ctx, id, "pre_replacement", j.Op.ID); err != nil {
				return err
			}
			if err = j.Stage(ctx, "delete_old", ""); err != nil {
				return err
			}
			if _, err = s.Runtime.Delete(ctx, old.RuntimeName); err != nil {
				return err
			}
		case "stopped", "created":
		default:
			return errors.New("unknown old execution cannot be fenced")
		}
		latest, err := s.Runtime.Observe(ctx, old.RuntimeName)
		if o.State == "paused" {
			if !errors.Is(err, rt.ErrNotFound) {
				return errors.New("old paused execution still present or unreachable")
			}
			fence = old.RuntimeName + ":absent"
		} else {
			if err != nil || (latest.State != "stopped" && latest.State != "created") {
				return errors.New("old execution did not stop")
			}
			fence = old.RuntimeName + ":" + latest.State
		}
	}
	f, err := os.Open(filepath.Join(cat.Root, item.ID, snapshots.CheckpointFile))
	if err != nil {
		return err
	}
	defer f.Close()
	if err = j.Stage(ctx, "import_candidate", ""); err != nil {
		return err
	}
	if _, err = s.Runtime.RestoreCheckpoint(ctx, j.Binding.RuntimeName, f, item.Size); err != nil {
		return err
	}
	j.Written = true
	if _, err = s.Runtime.SetPorts(ctx, j.Binding.RuntimeName, []rt.Port{}); err != nil {
		return err
	}
	labels := map[string]*string{}
	for k, v := range map[string]string{"platform.machine": m.ID, "platform.binding": j.Binding.ID, "platform.spec": j.Binding.SpecHash} {
		val := v
		labels[k] = &val
	}
	if _, err = s.Runtime.SetLabels(ctx, j.Binding.RuntimeName, labels); err != nil {
		return err
	}
	o, err := s.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if o.State != "created" || len(o.Ports) != 0 || o.Labels["platform.binding"] != j.Binding.ID || o.ParentMachine != "" {
		return errors.New("candidate creation, ownership or port fence not verified")
	}
	if err = j.Stage(ctx, "start_candidate", ""); err != nil {
		return err
	}
	if _, err = s.Runtime.Start(ctx, j.Binding.RuntimeName); err != nil {
		return err
	}
	method := "complete_copy"
	if j.Op.Kind == "rollback" {
		method = "complete_recovery"
	}
	reply, err := s.Call(ctx, j.Binding, tasks.Request{Method: method, Source: &capture.Source, OperationID: j.Op.ID, Policy: v.Disposition, StopEvidence: fence})
	if err != nil {
		return err
	}
	if reply.Binding == nil || *reply.Binding != *lifecycle.GuestBinding(j.Binding) {
		return errors.New("candidate guest ownership not confirmed")
	}
	if _, err = s.InstallSSH(ctx, j.Binding); err != nil {
		return err
	}
	if _, err = s.Runtime.EnablePublicNetwork(ctx, j.Binding.RuntimeName); err != nil {
		return err
	}
	o, err = s.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if o.State != "running" || !o.Identity.Complete() || o.Labels["platform.binding"] != j.Binding.ID {
		return errors.New("candidate runtime identity not confirmed")
	}
	x := lifecycle.Evidence(o)
	x.GuestManagementStatus = "ready"
	x.GuestManagementEvidence = lifecycle.JSON(reply.Binding)
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"machine_id": m.ID, "snapshot_id": item.ID}), PromoteBinding: true, Observation: &x, DesiredPower: "running", Apply: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE snapshot_pins SET owner_kind='binding',owner_id=? WHERE owner_kind='operation' AND owner_id=? AND snapshot_id=?", j.Binding.ID, j.Op.ID, item.ID); err != nil {
			return err
		}
		if old != nil && old.ID != j.Binding.ID && j.Op.Kind == "clone" {
			if _, err := tx.ExecContext(ctx, "INSERT INTO dependencies(parent_binding_id,child_binding_id,kind,status,observed_at) VALUES(?,?,'logical_origin','active',?)", old.ID, j.Binding.ID, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return lifecycle.SaveAdopted(ctx, tx, j.Binding, reply.Tasks)
	}})
}
