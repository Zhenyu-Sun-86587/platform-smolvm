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
	"reflect"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
	"sandbox-platform/internal/tasks"
	"time"
)

type copyRequest struct {
	CaptureMode         snapshots.CaptureMode  `json:"capture_mode,omitempty"`
	SnapshotID          string                 `json:"snapshot_id,omitempty"`
	Label               string                 `json:"label,omitempty"`
	Purpose             string                 `json:"purpose,omitempty"`
	ExpectedGeneration  int64                  `json:"expected_generation,omitempty"`
	CopyPolicy          string                 `json:"copy_policy,omitempty"`
	ChildName           string                 `json:"child_name,omitempty"`
	Name                string                 `json:"name,omitempty"`
	Disposition         string                 `json:"task_disposition,omitempty"`
	OldStatePolicy      string                 `json:"old_state_policy,omitempty"`
	VolumeRestorePolicy rt.VolumeRestorePolicy `json:"volume_restore_policy,omitempty"`
	NetworkMode         string                 `json:"network_mode,omitempty"`
}

func (s *Server) snapshotRoutes() {
	for _, k := range []string{"snapshot", "clone", "restore", "rollback"} {
		s.Extra[k] = s.copyJob
	}
	for _, k := range []string{"clone", "restore"} {
		s.Recover[k] = s.recoverImport
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
			if kind != "snapshot" && v.CaptureMode != "" {
				return registry.ErrInvalid
			}
			if kind != "restore" && kind != "rollback" && (v.VolumeRestorePolicy != "" || v.NetworkMode != "") {
				return registry.ErrInvalid
			}
			switch kind {
			case "snapshot":
				switch v.CaptureMode {
				case snapshots.CaptureVM:
					if v.ExpectedGeneration < 1 || v.Purpose != "" && v.Purpose != "recovery" && v.Purpose != "clone" {
						return registry.ErrInvalid
					}
					req.Kind = rt.VMSnapshot
				case snapshots.CaptureManagedTasks:
					v.CaptureMode = ""
				case "":
				default:
					return registry.ErrInvalid
				}
				if req.Kind == rt.VMSnapshot {
					if err := s.machineStatePointProfile(r.Context(), mid, req.Kind); err != nil {
						return err
					}
					break
				}
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
				if item.VM != nil {
					if err := p.Authorize(auth.ActionManage, item.MachineID); err != nil {
						return err
					}
					if item.State != "ready" || (v.VolumeRestorePolicy != "" && v.VolumeRestorePolicy != rt.RestoreExact &&
						v.VolumeRestorePolicy != rt.RestoreCloneConflicts) || (v.NetworkMode != "" && v.NetworkMode != "snapshot") {
						return registry.ErrInvalid
					}
					if v.VolumeRestorePolicy == "" {
						v.VolumeRestorePolicy = rt.RestoreCloneConflicts
					}
					if kind == "rollback" {
						if item.MachineID != mid {
							return registry.ErrConflict
						}
						if v.ExpectedGeneration < 1 {
							return registry.ErrInvalid
						}
						req.Kind = rt.VMRollback
						if err := s.machineStatePointProfile(r.Context(), mid, req.Kind); err != nil {
							return err
						}
						return s.accept(w, r, p, v, req)
					}
					if !validChildName(v.Name) {
						return registry.ErrInvalid
					}
					route := registry.Binding{RuntimeInstanceID: item.VM.RuntimeInstanceID, RuntimeBundleID: item.VM.RuntimeBundleID}
					if err := s.requireStatePointProfile(r.Context(), route, rt.VMRestore); err != nil {
						return err
					}
					source, err := s.DB.GetMachine(r.Context(), item.MachineID)
					if err != nil {
						return err
					}
					req.Kind, req.Name, req.TemplateID = rt.VMRestore, v.Name, source.TemplateID
					req.RuntimeInstanceID, req.MachineSpec = item.VM.RuntimeInstanceID, source.Spec
					req.DefaultIdlePolicy = lifecycle.JSON(lifecycle.IdlePolicy{Enabled: false})
					req.DesiredPower = "running"
					return s.accept(w, r, p, v, req)
				}
				if v.VolumeRestorePolicy != "" || v.NetworkMode != "" {
					return registry.ErrInvalid
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
	listSnapshots := func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if id == "" {
			id = p.Scope.MachineID
		}
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		out, err := s.Snapshots.List(r.Context(), id)
		if err != nil {
			return err
		}
		return list(w, r, out, func(s snapshots.Snapshot) string { return s.ID })
	}
	s.route("GET /v1/machines/{machine}/snapshots", listSnapshots)
	s.route("GET /v1/snapshots", listSnapshots)
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
		if item.VM != nil {
			return rt.ErrUnsupported
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
		if err := j.CheckSameRuntime(*source); err != nil {
			return err
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
	adapter, err := s.ForBinding(b)
	if err != nil {
		return "", err
	}
	m, err := s.DB.GetMachine(ctx, b.MachineID)
	if err != nil {
		return "", err
	}
	tpl, err := s.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil {
		return "", err
	}
	observed, err := adapter.Observe(ctx, b.RuntimeName)
	if err != nil {
		return "", err
	}
	if observed.Labels["platform.binding"] != b.ID {
		return "", registry.ErrStaleObservation
	}
	if observed.Capabilities["checkpoint"].ReasonCode == "UNSUPPORTED_PROFILE" || len(observed.EgressRules) != 0 {
		return "", &lifecycle.Failure{Code: "UNSUPPORTED_PROFILE", Message: "checkpoint requires the internal-storage profile and basic egress policy"}
	}
	if paused && observed.State != rt.StatePaused {
		return "", &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "saved execution changed before export"}
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
			probe, probeErr := j.Call(ctx, b, tasks.Request{Method: "hello"})
			if probeErr != nil || probe.Hello == nil || probe.Hello.Barrier != nil && probe.Hello.Barrier.OperationID == j.Op.ID {
				j.Written = true
			}
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
			if b.PauseStatus == "active" && b.RuntimePauseID != "" && !observed.PressurePaused() {
				_, err = adapter.ExportPausedCheckpoint(ctx, b.RuntimeName, pw, b.RuntimePauseID)
			} else {
				_, err = adapter.ExportPausedCheckpoint(ctx, b.RuntimeName, pw)
			}
		} else {
			_, err = adapter.CaptureCheckpoint(ctx, b.RuntimeName, pw)
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

// recoverImport can continue only before any candidate Start intent was written.
// An operator can repair the status helper and reobserve the same imported name.
func (s *Server) recoverImport(ctx context.Context, j *lifecycle.Job) error {
	if j.Intent.Stage != "import_candidate" {
		return errors.New("candidate stage needs independent outcome evidence")
	}
	var v copyRequest
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	var pinned string
	var count int
	if err := s.DB.ReadDB().QueryRowContext(ctx, "SELECT count(*),COALESCE(MIN(snapshot_id),'') FROM snapshot_pins WHERE owner_kind='operation' AND owner_id=?", j.Op.ID).Scan(&count, &pinned); err != nil {
		return err
	}
	if count != 1 || (v.SnapshotID != "" && v.SnapshotID != pinned) {
		return errors.New("original import pin is not unique")
	}
	v.SnapshotID = pinned
	return s.restore(ctx, j, nil, v)
}
func (s *Server) restore(ctx context.Context, j *lifecycle.Job, old *registry.Binding, v copyRequest) error {
	cat := s.Snapshots
	item, err := cat.Get(ctx, v.SnapshotID)
	if err != nil {
		return err
	}
	if item.VM != nil {
		return rt.ErrUnsupported
	}
	if item.State != "ready" || item.Content == "unknown" || (j.Op.Kind != "rollback" && item.Content != "clean") {
		return registry.ErrConflict
	}
	if err = cat.Pin(ctx, item.ID, "operation", j.Op.ID); err != nil {
		return err
	}
	recovering := j.Intent.Stage == "import_candidate"
	keepPin := recovering
	defer func() {
		if !keepPin {
			_ = cat.Unpin(context.WithoutCancel(ctx), item.ID, "operation", j.Op.ID)
		}
	}()
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
	var fence rollbackFence
	if j.Op.Kind == "rollback" {
		if old == nil || capture.Source.MachineID != j.Op.MachineID {
			return registry.ErrGeneration
		}
		fence, err = s.fenceOld(ctx, j, *old, v.OldStatePolicy)
		if err != nil {
			keepPin = j.Written || errors.Is(err, rt.ErrUncertain)
			return err
		}
		keepPin = j.Written
	}
	if !recovering {
		f, err := os.Open(filepath.Join(cat.Root, item.ID, snapshots.CheckpointFile))
		if err != nil {
			return err
		}
		defer f.Close()
		if err = j.Stage(ctx, "import_candidate", ""); err != nil {
			return err
		}
		if _, err = j.Runtime.RestoreCheckpoint(ctx, j.Binding.RuntimeName, f, item.Size); err != nil {
			keepPin = keepPin || errors.Is(err, rt.ErrUncertain)
			return err
		}
		keepPin = true
		j.Written = true
		if _, err = j.Runtime.SetPorts(ctx, j.Binding.RuntimeName, []rt.Port{}); err != nil {
			return err
		}
		labels := map[string]*string{}
		for k, v := range map[string]string{"platform.machine": m.ID, "platform.binding": j.Binding.ID, "platform.spec": j.Binding.SpecHash} {
			val := v
			labels[k] = &val
		}
		if _, err = j.Runtime.SetLabels(ctx, j.Binding.RuntimeName, labels); err != nil {
			return err
		}
	}
	o, err := j.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if o.State != "created" || len(o.Ports) != 0 || o.Labels["platform.binding"] != j.Binding.ID || o.Labels["platform.machine"] != j.Binding.MachineID || o.Labels["platform.spec"] != j.Binding.SpecHash ||
		o.ParentMachine != "" || o.AllowedCIDRs == nil || o.AllowedHosts == nil || len(o.AllowedCIDRs) != 0 ||
		len(o.AllowedHosts) != 0 || len(o.EgressRules) != 0 {
		return errors.New("candidate creation, ownership, port or egress fence not verified")
	}
	if err = s.checkRestartPolicy(ctx, j.Binding, o.State); err != nil {
		return err
	}
	if j.Op.Kind == "rollback" {
		if err = s.verifyFence(ctx, fence); err != nil {
			return err
		}
	}
	if err = j.Stage(ctx, "start_candidate", ""); err != nil {
		return err
	}
	if _, err = j.Runtime.Start(ctx, j.Binding.RuntimeName); err != nil {
		return err
	}
	method := "complete_copy"
	if j.Op.Kind == "rollback" {
		method = "complete_recovery"
	}
	if err = candidateReady(ctx, 20*time.Second, func(ctx context.Context) error {
		hello, e := s.Call(ctx, j.Binding, tasks.Request{Method: "hello"})
		if e != nil {
			return e
		}
		if hello.Binding == nil || *hello.Binding != capture.Source || hello.Hello == nil || !hello.Hello.Cgroup ||
			hello.Hello.Barrier == nil || !reflect.DeepEqual(*hello.Hello.Barrier, capture) {
			return errors.New("candidate did not preserve source task barrier")
		}
		return nil
	}); err != nil {
		return err
	}
	stopEvidence := ""
	if j.Op.Kind == "rollback" {
		stopEvidence = fence.evidence()
	}
	reply, err := s.Call(ctx, j.Binding, tasks.Request{Method: method, Source: &capture.Source, OperationID: j.Op.ID, Policy: v.Disposition, StopEvidence: stopEvidence})
	if err != nil {
		return err
	}
	if reply.Binding == nil || *reply.Binding != *lifecycle.GuestBinding(j.Binding) {
		return errors.New("candidate guest ownership not confirmed")
	}
	keyRev, err := s.SSHKeyRevision(ctx)
	if err != nil {
		return err
	}
	pub, err := s.InstallSSH(ctx, j.Binding)
	if err != nil {
		return err
	}
	if j.Op.Kind == "rollback" {
		if err = j.RepublishServices(ctx); err != nil {
			return err
		}
	}
	if _, err = j.Runtime.EnablePublicNetwork(ctx, j.Binding.RuntimeName); err != nil {
		return err
	}
	o, err = j.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if o.State != "running" || !o.Identity.Complete() || o.Labels["platform.binding"] != j.Binding.ID {
		return errors.New("candidate runtime identity not confirmed")
	}
	x := lifecycle.Evidence(o)
	x.GuestManagementStatus = "ready"
	x.GuestManagementEvidence = lifecycle.JSON(map[string]any{"task_manager": reply.Binding, "ssh_ready": true, "ssh_host_key": pub, "ssh_key_revision": keyRev})
	result := map[string]string{"machine_id": m.ID, "snapshot_id": item.ID}
	if fence.artifact != "" {
		result["pre_replacement_snapshot_id"] = fence.artifact
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(result), PromoteBinding: true, Observation: &x, DesiredPower: "running", Apply: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE snapshot_pins SET owner_kind='binding',owner_id=? WHERE owner_kind='operation' AND owner_id=? AND snapshot_id=?", j.Binding.ID, j.Op.ID, item.ID); err != nil {
			return err
		}
		if old != nil && old.ID != j.Binding.ID && j.Op.Kind == "clone" {
			if _, err := tx.ExecContext(ctx, "INSERT INTO dependencies(parent_binding_id,child_binding_id,kind,status,observed_at) VALUES(?,?,'logical_origin','active',?)", old.ID, j.Binding.ID, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		if fence.state == "absent" {
			if _, err := tx.ExecContext(ctx, "UPDATE port_allocations SET state='released' WHERE binding_id=?", fence.binding.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM snapshot_pins WHERE owner_kind='binding' AND owner_id=?", fence.binding.ID); err != nil {
				return err
			}
		}
		if j.Op.Kind == "rollback" {
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='interrupted',observed_at=?,result_json=
				json_set(result_json,'$.state','interrupted','$.reason','binding_retired_after_fence','$.observed_at',?)
				WHERE machine_id=? AND binding_generation=? AND state NOT IN ('succeeded','failed','cancelled','interrupted')`,
				now.UnixMilli(), now.Format(time.RFC3339Nano), fence.binding.MachineID, fence.binding.Generation); err != nil {
				return err
			}
		}
		return lifecycle.SaveAdopted(ctx, tx, j.Binding, reply.Tasks)
	}})
}
