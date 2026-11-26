package platformapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type remoteGuestRelayKey struct{}

type remoteGuestRelayListener struct {
	ctx    context.Context
	cancel context.CancelFunc
	scope  rt.GuestRelayScope
	bridge rt.GuestRelayBridge
}

func (l *remoteGuestRelayListener) Addr() net.Addr { return remoteGuestRelayAddr(l.scope.BindingID) }

func (l *remoteGuestRelayListener) Close() error {
	l.cancel()
	return nil
}

func (l *remoteGuestRelayListener) Accept() (net.Conn, error) {
	for l.ctx.Err() == nil {
		if conn, err := l.bridge.AcceptGuestRelay(l.ctx, l.scope); err == nil {
			return conn, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-l.ctx.Done():
			timer.Stop()
		}
	}
	return nil, net.ErrClosed
}

type remoteGuestRelayAddr string

func (a remoteGuestRelayAddr) Network() string { return "nodegateway-guest" }
func (a remoteGuestRelayAddr) String() string  { return string(a) }

func (s *Server) startRemoteGuestRelay(ctx context.Context, binding registry.Binding, bridge rt.GuestRelayBridge) *http.Server {
	listenerCtx, cancel := context.WithCancel(ctx)
	scope := rt.GuestRelayScope{BindingID: binding.ID, Name: binding.RuntimeName, Execution: binding.ExecutionRef()}
	listener := &remoteGuestRelayListener{ctx: listenerCtx, cancel: cancel, scope: scope, bridge: bridge}
	verify := func(r *http.Request) (string, error) {
		ctx := r.Context()
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		proof, _ := ctx.Value(remoteGuestRelayKey{}).(rt.GuestRelayScope)
		if !ok || proof != scope || bridge.AuthorizeGuestRelay(ctx) != nil {
			return "", auth.ErrUnauthenticated
		}
		machine, err := s.DB.GetMachine(ctx, binding.MachineID)
		if err != nil || machine.DeletedAt != nil || machine.WorkerID != binding.WorkerID {
			return "", auth.ErrUnauthenticated
		}
		current := machine.CurrentBinding
		if current == nil || current.ID != scope.BindingID || current.BundleRoute() != binding.BundleRoute() ||
			current.ExecutionRef() != scope.Execution || current.PowerState != registry.PowerRunning {
			return "", auth.ErrUnauthenticated
		}
		return secret, nil
	}
	server := &http.Server{
		Handler: s.guestRelayHandler(guestContext(binding), verify), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 * 1024,
		BaseContext:    func(net.Listener) context.Context { return listenerCtx },
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return context.WithValue(ctx, remoteGuestRelayKey{}, scope)
		},
	}
	// Each request receives its own authenticated stream. This confines the
	// attestation to the relay API invocation and its grant/action checks.
	server.SetKeepAlivesEnabled(false)
	return runGuestRelay(server, listener)
}
