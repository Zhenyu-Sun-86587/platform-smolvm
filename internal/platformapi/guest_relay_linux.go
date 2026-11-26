package platformapi

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type guestTransportKey struct{}

// GuestRelay serves one host-owned listener associated with a registered VMM.
// The listener is configured by the runtime's host side, outside the public API.
func (s *Server) GuestRelay(workerSocket string, binding registry.Binding) *http.Server {
	worker := workerclient.Client{Socket: workerSocket, Bundle: binding.RuntimeBundleID}
	execution := workerclient.Execution{
		InstanceID: binding.RuntimeInstanceObjectID, IncarnationID: binding.IncarnationID,
		Epoch: binding.ExecutionEpoch,
	}
	peer := guestContext(binding)
	verify := func(r *http.Request) (string, error) {
		connection, _ := r.Context().Value(guestTransportKey{}).(*net.UnixConn)
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || connection == nil || worker.VerifyGuestTransport(r.Context(), execution, connection) != nil {
			return "", auth.ErrUnauthenticated
		}
		return secret, nil
	}
	return &http.Server{
		Handler: s.guestRelayHandler(peer, verify), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 * 1024,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			if unix, ok := connection.(*net.UnixConn); ok {
				return context.WithValue(ctx, guestTransportKey{}, unix)
			}
			return ctx
		},
	}
}

func (s *Server) guestRelayHandler(peer auth.AttestedGuestContext, verify guestVerifier) http.Handler {
	mux := http.NewServeMux()
	s.guestDelegationRoutes(mux, peer, verify)
	mux.HandleFunc("/v1/credential-proxies/{proxy}/{route...}", guestHandler(verify, func(w http.ResponseWriter, r *http.Request, secret string) error {
		return s.serveCredentialProxy(w, r, peer, secret, verify)
	}))
	mux.HandleFunc("GET /v1/machines/{machine}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		secret, err := verify(r)
		if err != nil {
			fail(w, r, err)
			return
		}
		var view machineView
		target := r.PathValue("machine")
		err = s.Auth.UseGuestGrant(r.Context(), peer, secret, auth.GuestRead, target, func(_ *sql.Tx, _ auth.GuestPrincipal) error {
			machine, err := s.DB.GetMachine(r.Context(), target)
			if err != nil {
				return err
			}
			if machine.DeletedAt != nil {
				return registry.ErrNotFound
			}
			view, err = s.viewMachine(r.Context(), machine, false)
			return err
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		_ = write(w, http.StatusOK, view)
	})
	mux.HandleFunc("POST /v1/machines/{machine}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		secret, err := verify(r)
		if err == nil {
			err = s.guestRelayExec(w, r, peer, secret, verify)
		}
		if err != nil {
			fail(w, r, err)
		}
	})
	return mux
}

func (s *Server) guestRelayExec(w http.ResponseWriter, r *http.Request, peer auth.AttestedGuestContext, secret string, verify func(*http.Request) (string, error)) error {
	var request struct {
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
		Cwd     string            `json:"cwd,omitempty"`
		Stdin   string            `json:"stdin,omitempty"`
		Timeout uint64            `json:"timeout_seconds,omitempty"`
	}
	if err := decode(r, &request); err != nil {
		return err
	}
	if len(request.Command) == 0 || request.Timeout > 120 {
		return registry.ErrInvalid
	}

	ctx := r.Context()
	target := r.PathValue("machine")
	var scope registry.Binding
	var control rt.NativeGuestControl
	authorized, release, err := s.Auth.BeginGuestCall(ctx, peer, secret, auth.GuestExec, target, 125*time.Second, 0, func(tx *sql.Tx, guest auth.GuestPrincipal) error {
		machine, err := s.DB.GetMachine(ctx, target)
		if err != nil {
			return err
		}
		if machine.CurrentBinding == nil || machine.DeletedAt != nil || machine.CurrentBinding.PowerState != registry.PowerRunning || machine.CurrentBinding.ExecutionEpoch < 1 {
			return registry.ErrStaleObservation
		}
		scope = *machine.CurrentBinding
		adapter, err := s.ForBinding(scope)
		if err != nil {
			return err
		}
		var ok bool
		control, ok = adapter.(rt.NativeGuestControl)
		if !ok {
			return rt.ErrUnsupported
		}
		return registry.AppendEvent(ctx, tx, "auth", target, "", map[string]string{"action": "guest_exec_admitted", "grant_id": guest.GrantID}, true)
	})
	if err != nil {
		return err
	}
	defer release()
	check := func(ctx context.Context) error {
		if _, err := verify(r.WithContext(ctx)); err != nil {
			return err
		}
		if err := s.Auth.CheckGuestGrant(ctx, peer, secret, authorized); err != nil {
			return err
		}
		latest, err := s.DB.GetMachine(ctx, target)
		if err != nil {
			return err
		}
		if current := latest.CurrentBinding; current == nil || current.ID != scope.ID || current.ExecutionRef() != scope.ExecutionRef() || current.PowerState != registry.PowerRunning {
			return registry.ErrStaleObservation
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return err
	}
	stopActivity, err := s.holdGuestRelayActivity(ctx, scope, authorized.GrantID, "guest_exec")
	if err != nil {
		return err
	}
	defer stopActivity()
	if err := check(ctx); err != nil {
		return err
	}
	call, cancel := watchGuestAuthority(ctx, check)
	defer cancel(nil)
	result, err := control.GuestExec(call, scope.ExecutionRef(), rt.ExecRequest{
		Command: request.Command, Env: request.Env, Cwd: request.Cwd, Stdin: request.Stdin, TimeoutSeconds: request.Timeout,
	})
	if cause := context.Cause(call); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}
	if err := check(ctx); err != nil {
		return err
	}
	return write(w, http.StatusOK, result)
}

func (s *Server) holdGuestRelayActivity(ctx context.Context, binding registry.Binding, grant, reason string) (func(), error) {
	adapter, err := s.ForBinding(binding)
	if err != nil {
		return nil, err
	}
	client, supported := lifecycle.PolicyClient(adapter, binding)
	if !supported {
		return func() {}, nil
	}
	if _, err := s.Engine.ApplyMachinePolicy(ctx, binding, client); err != nil {
		return nil, err
	}
	return s.Engine.HoldConnectionActivity(ctx, binding, client, grant, reason)
}
