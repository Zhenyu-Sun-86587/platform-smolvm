package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const managedSSHProtocol = "sandbox.tunnel.v1"

// Managed SSH is an intrinsic System management path. The manage permission
// already authorizes root Guest exec. Publication permissions and settings
// continue to govern the unflagged SSH tunnel.
func (s *Server) managedSSHTunnel(w http.ResponseWriter, r *http.Request, mid string) error {
	if r.Header.Get("Authorization") == "" {
		return auth.ErrUnauthenticated
	}
	p, err := s.streamPrincipal(r, "")
	if err != nil {
		return err
	}
	if err = p.Authorize(auth.ActionManage, mid); err != nil {
		return err
	}
	protocols := strings.Split(strings.Join(r.Header.Values("Sec-WebSocket-Protocol"), ","), ",")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !slices.ContainsFunc(protocols, func(proto string) bool {
		return strings.TrimSpace(proto) == managedSSHProtocol
	}) {
		return registry.ErrInvalid
	}
	admission, cancel := context.WithTimeout(r.Context(), time.Duration(s.Config.Gateway.WaitSeconds)*time.Second)
	defer cancel()
	before, err := s.ManagedSystemSSHBinding(admission, mid)
	if errors.Is(err, rt.ErrUnsupported) {
		return managedSSHCapability("MANAGED_SSH_PROFILE_REQUIRED", "managed SSH requires a qualified native System template")
	}
	if err != nil {
		return err
	}
	adapter, err := s.ForBinding(before)
	if err != nil {
		return err
	}
	worker, ok := lifecycle.PolicyClient(adapter, before)
	if !ok {
		return managedSSHCapability("MANAGED_SSH_WORKER_REQUIRED", "managed SSH requires the native Worker access controller")
	}
	if err = s.checkManagedSSHNetwork(admission, worker, before); err != nil {
		return err
	}
	// Wake may promote the execution epoch, while replacing the binding or its
	// runtime slot requires a fresh request.
	reauthorize := func(ctx context.Context) error {
		_, err := s.currentManagedSSHBinding(ctx, before)
		return err
	}
	b, release, err := s.AccessAt(admission, p, mid, lifecycle.AccessEndpoint{
		Trigger: "api_access", Revision: 1, Reauthorize: reauthorize,
	})
	if err != nil {
		return err
	}
	defer release()
	if err = reauthorize(admission); err != nil {
		return err
	}
	key, err := s.awaitNativeSSHKey(admission, p, b, reauthorize)
	if err != nil {
		return err
	}
	attachment, err := managedSSHAttachment(admission, worker, b)
	if err != nil {
		return err
	}
	check := func(ctx context.Context) error {
		if err := s.checkManagedSSHExecution(ctx, p, b, key); err != nil {
			return err
		}
		current, err := managedSSHAttachment(ctx, worker, b)
		if err != nil {
			return err
		}
		if current != attachment {
			return registry.ErrGeneration
		}
		return nil
	}
	backend := gateway.Backend{Port: lifecycle.SystemSSHPort, Protocol: "ssh", DialPrivate: func(ctx context.Context) (net.Conn, error) {
		conn, err := worker.DialAttachment(ctx, attachment, lifecycle.SystemSSHPort, "tcp")
		if err != nil {
			return nil, err
		}
		if err = s.checkManagedSSHExecution(ctx, p, b, key); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	if err = s.checkManagedSSHExecution(admission, p, b, key); err != nil {
		return err
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{managedSSHProtocol}})
	if err != nil {
		return nil
	}
	defer conn.CloseNow()
	if err = backend.Tunnel(r.Context(), conn, check); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "managed SSH backend closed")
	}
	return nil
}

func (s *Server) currentManagedSSHBinding(ctx context.Context, before registry.Binding) (registry.Binding, error) {
	m, err := s.DB.GetMachine(ctx, before.MachineID)
	if err != nil {
		return registry.Binding{}, err
	}
	b := m.CurrentBinding
	if m.DeletedAt != nil || b == nil || b.ID != before.ID || b.Generation != before.Generation || b.Status != "current" ||
		b.RuntimeInstanceID != before.RuntimeInstanceID || b.RuntimeBundleID != before.RuntimeBundleID ||
		b.RuntimeInstanceObjectID != before.RuntimeInstanceObjectID {
		return registry.Binding{}, registry.ErrGeneration
	}
	return *b, nil
}

func (s *Server) checkManagedSSHExecution(ctx context.Context, p auth.Principal, admitted registry.Binding, key string) error {
	if err := s.CheckAccessPrincipal(ctx, p, admitted.MachineID); err != nil {
		return err
	}
	current, err := s.currentManagedSSHBinding(ctx, admitted)
	if err != nil {
		return err
	}
	if current.ExecutionRef() != admitted.ExecutionRef() || current.PowerState != registry.PowerRunning {
		return registry.ErrGeneration
	}
	verified, err := s.VerifiedNativeSSHKey(ctx, current, lifecycle.SystemSSHPort)
	if err != nil {
		return err
	}
	if key == "" || verified != key {
		return registry.ErrGeneration
	}
	return nil
}

func (s *Server) checkManagedSSHNetwork(ctx context.Context, worker workerclient.Client, b registry.Binding) error {
	if b.PowerState == registry.PowerRunning || b.PowerState == registry.PowerStandby {
		// The Worker lookup is passive and also works when a standby attachment
		// is temporarily gated. Opening the gate is part of verified access.
		_, err := worker.LookupAttachment(ctx, lifecycle.WorkerExecution(b))
		return managedSSHNetworkError(err)
	}
	m, err := s.DB.GetMachine(ctx, b.MachineID)
	if err != nil {
		return err
	}
	var spec struct {
		Network *rt.LaunchNetwork `json:"network"`
	}
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return err
	}
	if spec.Network == nil || spec.Network.NetworkID == "" {
		return managedSSHCapability("MANAGED_SSH_PRIVATE_NETWORK_REQUIRED", "managed SSH requires a configured Worker private network")
	}
	return nil
}

func managedSSHAttachment(ctx context.Context, worker workerclient.Client, b registry.Binding) (workerclient.NetworkAttachment, error) {
	a, err := worker.LookupAttachment(ctx, lifecycle.WorkerExecution(b))
	if err != nil {
		return a, managedSSHNetworkError(err)
	}
	if a.Lease.State != "attached" || a.Lease.Gate != "open" {
		return a, &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "managed SSH attachment is not published for this execution"}
	}
	return a, nil
}

func managedSSHNetworkError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var fault *workerclient.Fault
	if errors.As(err, &fault) && fault.Code == "E_NOT_FOUND" {
		return managedSSHCapability("MANAGED_SSH_PRIVATE_NETWORK_REQUIRED", "managed SSH requires a Worker private network attachment")
	}
	return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "managed SSH attachment could not be verified for this execution"}
}

func managedSSHCapability(reason, message string) error {
	return &rt.CapabilityError{Capability: managedSSHAccessCapability, Status: rt.Capability{
		Status: "unsupported", ReasonCode: reason, Message: message,
	}}
}
