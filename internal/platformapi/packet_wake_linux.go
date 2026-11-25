package platformapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type packetWakeContext struct {
	client     workerclient.Client
	execution  workerclient.Execution
	endpoint   workerclient.PacketWakeEndpoint
	revision   int64
	generation int64
}

func (s *Server) packetWakeEndpoint(ctx context.Context, machineID string) (packetWakeContext, error) {
	var result packetWakeContext
	machine, err := s.DB.GetMachine(ctx, machineID)
	if err != nil {
		return result, err
	}
	binding := machine.CurrentBinding
	if machine.DeletedAt != nil || binding == nil || binding.RuntimeInstanceObjectID == "" {
		return result, registry.ErrNotFound
	}
	adapter, err := s.ForBinding(*binding)
	if err != nil {
		return result, err
	}
	launcher, ok := adapter.(interface {
		DiscoverLaunchBinding(context.Context, string) (rt.LaunchBinding, error)
	})
	socket := privateServiceWorker(adapter)
	if !ok || socket == "" {
		return result, rt.ErrUnsupported
	}
	observed, err := launcher.DiscoverLaunchBinding(ctx, binding.RuntimeName)
	if err != nil {
		return result, err
	}
	if observed.InstanceID != binding.RuntimeInstanceObjectID || observed.ExecutionEpoch < binding.ExecutionEpoch {
		return result, registry.ErrGeneration
	}
	result.client = workerclient.Client{Socket: socket, Bundle: binding.RuntimeBundleID}
	result.revision = machine.Revision
	result.generation = machine.BindingGeneration
	result.execution = workerclient.Execution{InstanceID: observed.InstanceID, Epoch: observed.ExecutionEpoch}
	if observed.IncarnationID != nil {
		result.execution.IncarnationID = *observed.IncarnationID
	}
	result.endpoint, err = result.client.LookupPacketWakeEndpoint(ctx, result.execution)
	return result, packetWakeError(err)
}

func packetWakeError(err error) error {
	var fault *workerclient.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "E_NOT_FOUND":
			return registry.ErrNotFound
		case "E_INVALID_REQUEST":
			return registry.ErrInvalid
		case "E_STALE_EXECUTION":
			return registry.ErrGeneration
		case "E_STALE_REVISION":
			return registry.ErrRevision
		case "E_PERMISSION_DENIED":
			return auth.ErrForbidden
		case "E_IDEMPOTENCY_CONFLICT":
			return registry.ErrIdempotency
		case "E_RESOURCE_LIMIT", "E_STATE_CONFLICT":
			return registry.ErrConflict
		case "E_CAPABILITY_UNSUPPORTED", "E_METHOD_UNSUPPORTED":
			return rt.ErrUnsupported
		}
		return rt.ErrUnavailable
	}
	return err
}

func (s *Server) privatePacketWakeRoutes() {
	const path = "/v1/machines/{machine}/network/wake-rules/{source}/{transport}/{port}"
	s.route("GET "+path, s.privatePacketWakeRule)
	s.route("PUT "+path, s.privatePacketWakeRule)
}

func (s *Server) privatePacketWakeRule(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	targetID, sourceID := r.PathValue("machine"), r.PathValue("source")
	action := auth.ActionRead
	if r.Method == http.MethodPut {
		action = auth.ActionManage
	}
	if err := p.Authorize(action, targetID); err != nil {
		return err
	}
	if err := p.Authorize(action, sourceID); err != nil {
		return err
	}
	transport := r.PathValue("transport")
	port, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || port == 0 || sourceID == targetID || (transport != "tcp" && transport != "udp") {
		return registry.ErrInvalid
	}
	ctx := r.Context()
	var change struct {
		Enabled            *bool  `json:"enabled"`
		ExpectedRevision   *int64 `json:"expected_revision"`
		ExpectedGeneration int64  `json:"expected_generation"`
	}
	var operation, digest string
	var machineRevision int64
	if r.Method == http.MethodPut {
		if err := decode(r, &change); err != nil {
			return err
		}
		machineRevision, err = revision(r)
		if err != nil {
			return err
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 256 || change.Enabled == nil || change.ExpectedRevision == nil || *change.ExpectedRevision < 0 {
			return registry.ErrInvalid
		}
		control, err := s.DB.Control(ctx)
		if err != nil {
			return err
		}
		if control.ControlEpoch != r.Header.Get("If-Control-Epoch") {
			return registry.ErrControlEpoch
		}
		if control.Maintenance {
			return registry.ErrMaintenance
		}
		operation = fmt.Sprintf("packet-%x", sha256.Sum256([]byte(p.ID+"\x00"+control.ControlEpoch+"\x00"+r.URL.EscapedPath()+"\x00"+key)))
		body, err := json.Marshal(change)
		if err != nil {
			return err
		}
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	}
	source, err := s.packetWakeEndpoint(ctx, sourceID)
	if err != nil {
		return err
	}
	target, err := s.packetWakeEndpoint(ctx, targetID)
	if err != nil {
		return err
	}
	if source.client.Socket != target.client.Socket || source.endpoint.NetworkID != target.endpoint.NetworkID {
		return auth.ErrForbidden
	}
	if r.Method == http.MethodPut {
		if change.ExpectedGeneration != target.generation {
			return registry.ErrGeneration
		}
		if machineRevision != target.revision {
			return registry.ErrRevision
		}
	}
	rule := workerclient.PacketWakeRule{SourceAttachmentID: source.endpoint.AttachmentID, TargetAttachmentID: target.endpoint.AttachmentID,
		Transport: transport, GuestPort: uint16(port), Enabled: change.Enabled, ExpectedRevision: change.ExpectedRevision}
	if change.Enabled != nil && *change.Enabled {
		rule.SourceExecution = &source.execution
	}
	result, err := target.client.PacketWakeRule(ctx, target.execution, rule, operation, digest)
	if err != nil {
		return packetWakeError(err)
	}
	return write(w, http.StatusOK, result)
}
