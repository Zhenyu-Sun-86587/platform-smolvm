package nodegateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) registerVolumeRPC(mux *http.ServeMux) {
	rpc(s, mux, nodeproto.VolumeConsumerPath, func(ctx context.Context, route Route, in nodeproto.Name) (rt.ExecutionRef, error) {
		consumer, err := require[rt.VolumeConsumerResolver](route)
		if err != nil {
			return rt.ExecutionRef{}, err
		}
		ref, err := consumer.DiscoverVolumeConsumer(ctx, in.Name)
		if errors.Is(err, rt.ErrVolumeRequiresStopped) {
			return rt.ExecutionRef{}, nativeRefused("E_VOLUME_REQUIRES_STOPPED")
		}
		return ref, err
	})
	scopedRPC(s, mux, nodeproto.VolumeObservePath, func(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.VolumeLookup) (rt.VolumeObservation, error) {
		if !nodeproto.ValidID(in.VolumeID) || !nodeproto.ValidID(in.OwnerID) {
			return rt.VolumeObservation{}, rt.ErrInvalid
		}
		control, err := require[rt.VolumeControl](route)
		if err != nil {
			return rt.VolumeObservation{}, err
		}
		out, err := control.ObserveVolume(ctx, in.VolumeID, in.OwnerID)
		if err == nil && !nodeproto.VolumeMatches(scope, in, out) {
			return rt.VolumeObservation{}, &nodeproto.Failure{Code: nodeproto.CodeRoute}
		}
		return out, err
	})
	serverRPC(s, mux, nodeproto.VolumeApplyPath, s.storage, 130*time.Second, true, s.applyVolume)
}

func validateVolume(in nodeproto.VolumeMutation) error {
	if !nodeproto.ValidID(in.VolumeID) || !nodeproto.ValidID(in.OwnerID) ||
		!nodeproto.ValidID(in.OperationID) || !nodeproto.ValidDigest(in.RequestDigest) || in.ExpectedRevision < 0 {
		return rt.ErrInvalid
	}
	c := in.Command
	for _, id := range []string{c.SourceSnapshotID, c.SnapshotID, c.AttachmentID, c.InstanceID, c.DeviceKey} {
		if id != "" && !nodeproto.ValidID(id) {
			return rt.ErrInvalid
		}
	}
	hasCapacity := c.CapacityBytes > 0 && c.CapacityBytes%512 == 0
	hasConsumer := c.InstanceID != "" && c.ExecutionEpoch != nil && *c.ExecutionEpoch >= 0 && c.AttachmentID != ""
	switch c.Action {
	case "create":
		if in.ExpectedRevision == 0 && hasCapacity &&
			(c.Lifecycle == "persistent" || c.Lifecycle == "ephemeral" || c.Lifecycle == "machine") {
			return nil
		}
	case "grow":
		if in.ExpectedRevision > 0 && hasCapacity {
			return nil
		}
	case "snapshot", "drop_snapshot":
		if in.ExpectedRevision > 0 && c.SnapshotID != "" {
			return nil
		}
	case "attach":
		if in.ExpectedRevision > 0 && hasConsumer && c.DeviceKey != "" && c.DeviceKey != "root" &&
			(c.CapturePolicy == "include" || c.CapturePolicy == "external") &&
			((c.Mode == "ro" && c.SnapshotID != "" && (c.ForkPolicy == "share_immutable" || c.ForkPolicy == "reject")) ||
				(c.Mode == "rw" && c.SnapshotID == "" && (c.ForkPolicy == "clone" || c.ForkPolicy == "reject"))) {
			return nil
		}
	case "detach":
		if in.ExpectedRevision > 0 && hasConsumer {
			return nil
		}
	case "delete":
		if in.ExpectedRevision > 0 {
			return nil
		}
	}
	return rt.ErrInvalid
}

func (s *Server) applyVolume(ctx context.Context, scope nodeproto.Route, route Route, in nodeproto.VolumeMutation) (rt.VolumeReceipt, error) {
	var noReceipt rt.VolumeReceipt
	if err := validateVolume(in); err != nil {
		return noReceipt, err
	}
	control, err := require[rt.VolumeControl](route)
	if err != nil {
		return noReceipt, err
	}
	backend, err := require[rt.NativeControl](route)
	if err != nil {
		return noReceipt, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return noReceipt, err
	}
	command, err := json.Marshal(in.Command)
	if err != nil {
		return noReceipt, err
	}
	id := in.OperationID
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	entry, send, err := s.journal.beginVolume(ctx, scope, in, digest)
	if err != nil {
		return noReceipt, err
	}
	if entry.Receipt != nil {
		return *entry.Receipt, nil
	}
	if entry.Refusal != "" {
		return noReceipt, nativeRefused(entry.Refusal)
	}
	if !send {
		// Query the pinned native operation. Its catalog phase does not
		// contain the original product volume receipt, which may be older
		// than today's volume revision. Preserve Unknown without Apply.
		_, _ = backend.QueryOperation(ctx, id)
		return noReceipt, uncertain()
	}
	dispatch, cancel := context.WithTimeout(context.WithoutCancel(ctx), 128*time.Second)
	defer cancel()
	out, err := control.ApplyVolume(dispatch, rt.VolumeMutation{
		OperationID: id, RequestDigest: in.RequestDigest, VolumeID: in.VolumeID, OwnerID: in.OwnerID,
		ExpectedRevision: in.ExpectedRevision, Command: command,
	})
	if err != nil {
		var fault *rt.Error
		proved := errors.As(err, &fault) && nodeproto.NativeRefusal(fault.NativeCode)
		observed, queryErr := backend.QueryOperation(dispatch, id)
		abortedStorage := proved && fault.NativeCode == "E_VOLUME_STORAGE_NOT_APPLIED" &&
			queryErr == nil && volumeOperationMatches(scope, in, observed) && observed.Phase == "ABORTED"
		if proved && (nativeNotFound(queryErr) || abortedStorage) {
			persist, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer finish()
			if err := s.journal.finishVolume(persist, id, nil, fault.NativeCode); err == nil {
				return noReceipt, nativeRefused(fault.NativeCode)
			}
		}
		return noReceipt, uncertain()
	}
	target := nodeproto.VolumeLookup{VolumeID: in.VolumeID, OwnerID: in.OwnerID}
	if out.Schema != "VolumeOperation/1" || out.OperationID != id || !nodeproto.VolumeMatches(scope, target, out.Volume) {
		return noReceipt, uncertain()
	}
	persist, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer finish()
	if err := s.journal.finishVolume(persist, id, &out, ""); err != nil {
		return noReceipt, uncertain()
	}
	return out, nil
}

func volumeOperationMatches(scope nodeproto.Route, in nodeproto.VolumeMutation, r rt.OperationReceipt) bool {
	return r.OperationID == in.OperationID && r.RouteID == "deep" && r.BundleDigest == scope.BundleDigest &&
		r.InstanceID == in.VolumeID && r.RequestDigest == in.RequestDigest && r.ExpectedIncarnationID == nil && r.ExpectedExecutionEpoch == 0
}
