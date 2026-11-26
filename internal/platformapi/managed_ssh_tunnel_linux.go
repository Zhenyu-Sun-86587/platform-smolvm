package platformapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/execbridge"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

const managedSSHProtocol = "sandbox.tunnel.v1"
const managedSSHKeyHeader = "X-Sandbox-SSH-Public-Key"

func managedSSHUserKey(h http.Header) (ssh.PublicKey, error) {
	values := h.Values(managedSSHKeyHeader)
	// Plain Ed25519 is always the same-sized OpenSSH blob. The exact encoding
	// excludes certs, authorized_keys options, comments and request ambiguity.
	if len(values) != 1 || len(values[0]) != 80 || !strings.HasPrefix(values[0], "ssh-ed25519 ") {
		return nil, registry.ErrInvalid
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(values[0]))
	if err != nil || key.Type() != ssh.KeyAlgoED25519 ||
		strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n") != values[0] {
		return nil, registry.ErrInvalid
	}
	return key, nil
}

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
	userKey, err := managedSSHUserKey(r.Header)
	if err != nil {
		return err
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
	check := func(ctx context.Context) error {
		return s.checkManagedSSHExecution(ctx, p, b, key)
	}
	if err := check(admission); err != nil {
		return err
	}
	agent, err := worker.OpenGuestAgent(admission, lifecycle.WorkerExecution(b))
	if err != nil {
		return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "the Worker could not open the current Guest Agent"}
	}
	if err := check(admission); err != nil {
		_ = agent.Close()
		return err
	}
	stream, err := execbridge.OpenManagedSSHD(admission, agent, userKey)
	if errors.Is(err, rt.ErrUnsupported) {
		return managedSSHCapability("MANAGED_SSH_AGENT_REQUIRED", "managed SSH requires a System Agent with bidirectional exec")
	}
	if err != nil {
		return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "the Guest Agent could not start managed SSH"}
	}
	defer stream.Close()
	if err := check(admission); err != nil {
		return err
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{managedSSHProtocol}})
	if err != nil {
		return nil
	}
	_ = gateway.TunnelConn(r.Context(), conn, stream, check)
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

func managedSSHCapability(reason, message string) error {
	return &rt.CapabilityError{Capability: managedSSHAccessCapability, Status: rt.Capability{
		Status: "unsupported", ReasonCode: reason, Message: message,
	}}
}
