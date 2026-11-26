package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/volumes"
)

type volumeIntent struct {
	rt.VolumeRoute
	VolumeID          string        `json:"volume_id,omitempty"`
	Name              string        `json:"name"`
	ExpectedRevision  int64         `json:"expected_revision"`
	Command           volumeCommand `json:"command"`
	RetiredAttachment *retiredLease `json:"retired_attachment,omitempty"`
}

type retiredLease struct {
	MachineID string `json:"machine_id"`
	BindingID string `json:"binding_id"`
}

type volumeCommand struct {
	Action           string `json:"action"`
	CapacityBytes    int64  `json:"capacity_bytes,omitempty"`
	Lifecycle        string `json:"lifecycle,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`
	SnapshotID       string `json:"snapshot_id,omitempty"`
	AttachmentID     string `json:"attachment_id,omitempty"`
	InstanceID       string `json:"instance_id,omitempty"`
	ExecutionEpoch   *int64 `json:"execution_epoch,omitempty"`
	DeviceKey        string `json:"device_key,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ForkPolicy       string `json:"fork_policy,omitempty"`
	CapturePolicy    string `json:"capture_policy,omitempty"`
}

type createVolume struct {
	Name              string `json:"name"`
	CapacityBytes     int64  `json:"capacity_bytes"`
	Lifecycle         string `json:"lifecycle,omitempty"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
}

func volumeName(name string) bool {
	return name != "" && len(name) <= 128 && strings.TrimSpace(name) == name && !strings.ContainsAny(name, "\x00\r\n")
}

func volumeCapacity(size int64) bool { return size > 0 && size%512 == 0 }

func (s *Server) volumeRoutes() {
	s.Extra[rt.VolumeOperation], s.Recover[rt.VolumeOperation] = s.volumeJob, s.volumeJob
	s.route("GET /v1/volumes", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		items, err := volumes.List(r.Context(), s.DB.ReadDB(), p.ID)
		if err != nil {
			return err
		}
		return list(w, r, items, func(v volumes.Volume) string { return v.ID })
	})
	s.route("POST /v1/volumes", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionManage, ""); err != nil {
			return err
		}
		var input createVolume
		if err := decode(r, &input); err != nil {
			return err
		}
		if !volumeName(input.Name) || !volumeCapacity(input.CapacityBytes) {
			return registry.ErrInvalid
		}
		if input.Lifecycle == "" {
			input.Lifecycle = "persistent"
		}
		if input.Lifecycle != "persistent" && input.Lifecycle != "ephemeral" {
			return registry.ErrInvalid
		}
		var route rt.VolumeRoute
		route.RuntimeInstanceID = input.RuntimeInstanceID
		if err := s.DB.ReadDB().QueryRowContext(r.Context(), `SELECT runtime_bundle_id FROM runtime_instances WHERE id=? AND status='registered'`,
			input.RuntimeInstanceID).Scan(&route.RuntimeBundleID); err != nil {
			return err
		}
		intent := volumeIntent{VolumeRoute: route, Name: input.Name,
			Command: volumeCommand{Action: "create", Lifecycle: input.Lifecycle, CapacityBytes: input.CapacityBytes}}
		return s.acceptVolume(w, r, p, intent, "", 0)
	})
	s.route("GET /v1/volumes/{volume}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		v, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("volume"))
		if err != nil {
			return err
		}
		if v.State == "deleted" {
			return registry.ErrNotFound
		}
		control, err := s.volumeControl(v.VolumeRoute)
		if err != nil {
			return err
		}
		observation, err := control.ObserveVolume(r.Context(), v.ID, v.OwnerID)
		if err != nil {
			return err
		}
		v.VolumeObservation = observation
		w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprint(v.Revision)))
		return write(w, 200, v)
	})
	s.route("PATCH /v1/volumes/{volume}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		var change struct {
			CapacityBytes int64 `json:"capacity_bytes"`
		}
		if err := decode(r, &change); err != nil {
			return err
		}
		if !volumeCapacity(change.CapacityBytes) {
			return registry.ErrInvalid
		}
		return s.existingVolume(w, r, p, r.PathValue("volume"), volumeCommand{Action: "grow", CapacityBytes: change.CapacityBytes})
	})
	s.route("DELETE /v1/volumes/{volume}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		return s.existingVolume(w, r, p, r.PathValue("volume"), volumeCommand{Action: "delete"})
	})
	s.route("GET /v1/volumes/{volume}/snapshots", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		v, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("volume"))
		if err != nil {
			return err
		}
		return list(w, r, v.Snapshots, func(v rt.VolumeSnapshot) string { return v.ID })
	})
	s.route("POST /v1/volumes/{volume}/snapshots", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		var body struct{}
		if err := decode(r, &body); err != nil {
			return err
		}
		return s.existingVolume(w, r, p, r.PathValue("volume"), volumeCommand{Action: "snapshot"})
	})
	s.route("GET /v1/volume-snapshots/{snap}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		v, err := volumes.GetSnapshot(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("snap"))
		if err != nil {
			return err
		}
		if v.State != "available" {
			return registry.ErrNotFound
		}
		return write(w, 200, v)
	})
	s.route("DELETE /v1/volume-snapshots/{snap}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionManage, ""); err != nil {
			return err
		}
		v, err := volumes.GetSnapshot(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("snap"))
		if err != nil {
			return err
		}
		return s.existingVolume(w, r, p, v.VolumeID, volumeCommand{Action: "drop_snapshot", SnapshotID: v.ID})
	})
	s.route("POST /v1/volume-snapshots/{snap}/clones", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionManage, ""); err != nil {
			return err
		}
		var input struct {
			Name string `json:"name"`
		}
		if err := decode(r, &input); err != nil {
			return err
		}
		if !volumeName(input.Name) {
			return registry.ErrInvalid
		}
		snapshot, err := volumes.GetSnapshot(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("snap"))
		if err != nil {
			return err
		}
		volume, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, snapshot.VolumeID)
		if err != nil {
			return err
		}
		intent := volumeIntent{VolumeRoute: volume.VolumeRoute, Name: input.Name, Command: volumeCommand{
			Action: "create", CapacityBytes: snapshot.CapacityBytes, Lifecycle: "persistent", SourceSnapshotID: snapshot.ID}}
		return s.acceptVolume(w, r, p, intent, "", 0)
	})
	s.volumeAttachmentRoutes()
}

func (s *Server) volumeControl(route rt.VolumeRoute) (rt.VolumeControl, error) {
	adapter, err := s.ForBinding(registry.Binding{RuntimeInstanceID: route.RuntimeInstanceID, RuntimeBundleID: route.RuntimeBundleID})
	if err != nil {
		return nil, err
	}
	control, ok := adapter.(rt.VolumeControl)
	if !ok {
		return nil, rt.ErrUnsupported
	}
	return control, nil
}

func (s *Server) acceptVolume(w http.ResponseWriter, r *http.Request, p auth.Principal, intent volumeIntent, machine string, generation int64) error {
	req := registry.AcceptRequest{Kind: rt.VolumeOperation, MachineID: machine, ExpectedGeneration: generation,
		Heavy: intent.Command.Action == "grow"}
	if machine == "" {
		req.RuntimeInstanceID = intent.RuntimeInstanceID
	}
	return s.accept(w, r, p, intent, req)
}

func (s *Server) existingVolume(w http.ResponseWriter, r *http.Request, p auth.Principal, id string, command volumeCommand) error {
	if err := p.Authorize(auth.ActionManage, ""); err != nil {
		return err
	}
	rev, err := revision(r)
	if err != nil {
		return err
	}
	v, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, id)
	if err != nil {
		return err
	}
	return s.acceptVolume(w, r, p, volumeIntent{VolumeRoute: v.VolumeRoute, VolumeID: v.ID, Name: v.Name,
		ExpectedRevision: rev, Command: command}, "", 0)
}

func (s *Server) volumeAttachmentRoutes() {
	s.route("GET /v1/machines/{machine}/volume-attachments", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		machine := r.PathValue("machine")
		if err := p.Authorize(auth.ActionRead, machine); err != nil {
			return err
		}
		items, err := volumes.ListAttachments(r.Context(), s.DB.ReadDB(), p.ID, machine)
		if err != nil {
			return err
		}
		for i := range items {
			item := &items[i]
			volume, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, item.VolumeID)
			if err != nil {
				return err
			}
			control, err := s.volumeControl(volume.VolumeRoute)
			if err != nil {
				return err
			}
			observation, err := control.ObserveVolume(r.Context(), item.VolumeID, p.ID)
			if err != nil {
				return err
			}
			item.State = "unknown"
			for _, lease := range observation.Attachments {
				if lease.ID == item.ID && lease.InstanceID == item.InstanceID {
					item.VolumeAttachment = lease
					break
				}
			}
		}
		return list(w, r, items, func(v volumes.Attachment) string { return v.ID })
	})
	s.route("POST /v1/machines/{machine}/volume-attachments", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		var input struct {
			VolumeID               string `json:"volume_id"`
			DeviceKey              string `json:"device_key"`
			Mode                   string `json:"mode"`
			SnapshotID             string `json:"snapshot_id,omitempty"`
			ForkPolicy             string `json:"fork_policy,omitempty"`
			CapturePolicy          string `json:"capture_policy,omitempty"`
			ExpectedVolumeRevision int64  `json:"expected_volume_revision"`
			ExpectedGeneration     int64  `json:"expected_generation"`
		}
		if err := p.Authorize(auth.ActionManage, r.PathValue("machine")); err != nil {
			return err
		}
		if err := decode(r, &input); err != nil {
			return err
		}
		if input.ExpectedVolumeRevision < 1 || input.ExpectedGeneration < 1 || input.DeviceKey == "" || input.DeviceKey == "root" {
			return registry.ErrInvalid
		}
		if input.Mode == "" {
			input.Mode = "rw"
		}
		if input.ForkPolicy == "" {
			if input.Mode == "ro" {
				input.ForkPolicy = "share_immutable"
			} else {
				input.ForkPolicy = "clone"
			}
		}
		if input.CapturePolicy == "" {
			input.CapturePolicy = "include"
		}
		if (input.Mode == "ro" && (input.SnapshotID == "" || (input.ForkPolicy != "share_immutable" && input.ForkPolicy != "reject"))) ||
			(input.Mode == "rw" && (input.SnapshotID != "" || (input.ForkPolicy != "clone" && input.ForkPolicy != "reject"))) ||
			(input.Mode != "rw" && input.Mode != "ro") || (input.CapturePolicy != "include" && input.CapturePolicy != "external") {
			return registry.ErrInvalid
		}
		volume, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, input.VolumeID)
		if err != nil {
			return err
		}
		intent := volumeIntent{VolumeRoute: volume.VolumeRoute, VolumeID: volume.ID, Name: volume.Name, ExpectedRevision: input.ExpectedVolumeRevision,
			Command: volumeCommand{Action: "attach", DeviceKey: input.DeviceKey, Mode: input.Mode, SnapshotID: input.SnapshotID, ForkPolicy: input.ForkPolicy, CapturePolicy: input.CapturePolicy}}
		return s.acceptVolume(w, r, p, intent, r.PathValue("machine"), input.ExpectedGeneration)
	})
	s.route("DELETE /v1/volume-attachments/{attachment}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		var input struct {
			ExpectedVolumeRevision int64 `json:"expected_volume_revision"`
			ExpectedGeneration     int64 `json:"expected_generation"`
		}
		if err := decode(r, &input); err != nil {
			return err
		}
		if input.ExpectedVolumeRevision < 1 || input.ExpectedGeneration < 1 {
			return registry.ErrInvalid
		}
		attachment, err := volumes.GetAttachment(r.Context(), s.DB.ReadDB(), p.ID, r.PathValue("attachment"))
		if err != nil {
			return err
		}
		if err := p.Authorize(auth.ActionManage, attachment.MachineID); err != nil {
			return err
		}
		volume, err := volumes.Get(r.Context(), s.DB.ReadDB(), p.ID, attachment.VolumeID)
		if err != nil {
			return err
		}
		intent := volumeIntent{VolumeRoute: volume.VolumeRoute, VolumeID: volume.ID, Name: volume.Name, ExpectedRevision: input.ExpectedVolumeRevision,
			Command: volumeCommand{Action: "detach", AttachmentID: attachment.ID}}
		machine, err := s.DB.GetMachine(r.Context(), attachment.MachineID)
		if err != nil {
			return err
		}
		if machine.DeletedAt != nil {
			rev, err := revision(r)
			if err != nil {
				return err
			}
			if machine.Revision != rev {
				return registry.ErrRevision
			}
			if machine.BindingGeneration != input.ExpectedGeneration {
				return registry.ErrGeneration
			}
			intent.RetiredAttachment = &retiredLease{MachineID: attachment.MachineID, BindingID: attachment.BindingID}
			return s.acceptVolume(w, r, p, intent, "", 0)
		}
		return s.acceptVolume(w, r, p, intent, attachment.MachineID, input.ExpectedGeneration)
	})
}

func (s *Server) volumeJob(ctx context.Context, job *lifecycle.Job) error {
	var intent volumeIntent
	if err := json.Unmarshal(job.Op.Request, &intent); err != nil {
		return err
	}
	control, ok := job.Runtime.(rt.VolumeControl)
	if !ok || intent.RuntimeInstanceID != job.Binding.RuntimeInstanceID || intent.RuntimeBundleID != job.Binding.RuntimeBundleID {
		return rt.ErrUnsupported
	}
	id := strings.TrimPrefix(job.Op.ID, "op_")
	if intent.VolumeID == "" {
		intent.VolumeID = "vol_" + id
	}
	command := intent.Command
	consumerBinding := job.Binding
	if retired := intent.RetiredAttachment; retired != nil {
		var err error
		consumerBinding, err = s.DB.GetBinding(ctx, retired.BindingID)
		if err != nil {
			return err
		}
		if command.Action != "detach" || consumerBinding.MachineID != retired.MachineID || consumerBinding.Status != "retired" ||
			consumerBinding.RuntimeInstanceID != intent.RuntimeInstanceID || consumerBinding.RuntimeBundleID != intent.RuntimeBundleID {
			return registry.ErrConflict
		}
	}
	if job.Intent.Volume != nil {
		if err := json.Unmarshal(job.Intent.Volume.Command, &command); err != nil {
			return err
		}
	}
	switch command.Action {
	case "snapshot":
		command.SnapshotID = "vsnap_" + id
	case "attach", "detach":
		if job.Intent.Volume == nil {
			consumer, ok := job.Runtime.(rt.VolumeConsumerResolver)
			if !ok || consumerBinding.PowerState == registry.PowerHibernated {
				return &lifecycle.Failure{Code: "VOLUME_TOPOLOGY_UNAVAILABLE", Message: "volume topology requires a stopped Worker-bound machine"}
			}
			ref, err := consumer.DiscoverVolumeConsumer(ctx, consumerBinding.RuntimeName)
			if err != nil {
				return err
			}
			if consumerBinding.RuntimeInstanceObjectID != "" &&
				(consumerBinding.RuntimeInstanceObjectID != ref.InstanceID || consumerBinding.ExecutionEpoch > ref.ExecutionEpoch) {
				return registry.ErrConflict
			}
			command.InstanceID = ref.InstanceID
			command.ExecutionEpoch = &ref.ExecutionEpoch
		}
		if command.Action == "attach" {
			command.AttachmentID = "vatt_" + id
		}
	}
	if job.Intent.Volume == nil {
		payload := lifecycle.JSON(command)
		digest := sha256.Sum256(job.Op.Request)
		job.Intent.Volume = &rt.VolumeMutation{OperationID: job.Op.ID, RequestDigest: fmt.Sprintf("sha256:%x", digest), VolumeID: intent.VolumeID,
			OwnerID: job.Op.PrincipalID, ExpectedRevision: intent.ExpectedRevision, Command: payload}
		if err := job.Stage(ctx, "volume_dispatch", intent.VolumeID); err != nil {
			return err
		}
	}
	receipt, err := control.ApplyVolume(ctx, *job.Intent.Volume)
	if err != nil {
		var runtimeFault *rt.Error
		if errors.As(err, &runtimeFault) {
			switch runtimeFault.NativeCode {
			case "E_VOLUME_STORAGE_NOT_APPLIED":
				return &lifecycle.Failure{Code: "VOLUME_STORAGE_NOT_APPLIED", Message: runtimeFault.Message}
			case "E_HOST_IO":
				err = fmt.Errorf("%w: %w", rt.ErrUncertain, err)
			}
		}
		job.Written = errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain)
		return err
	}
	job.Written = true
	return job.Done(ctx, registry.TransitionRequest{Result: lifecycle.JSON(receipt), Effect: registry.EffectApplied,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if err := volumes.Apply(ctx, tx, intent.VolumeRoute, intent.Name, consumerBinding.MachineID, consumerBinding.ID, command.AttachmentID, command.Action, receipt.Volume); err != nil {
				return err
			}
			volume := receipt.Volume
			return registry.AppendResourceEvent(ctx, tx, "operation", consumerBinding.MachineID, job.Op.ID, registry.ResourceEvent{
				Kind: "volume", ID: volume.ID, Name: "volume.state_changed", Audience: "owner", OwnerID: volume.OwnerID,
			}, lifecycle.JSON(map[string]any{"volume_id": volume.ID, "revision": volume.Revision, "state": volume.State, "action": command.Action}), false)
		}})
}
