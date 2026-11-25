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
	mux := http.NewServeMux()
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
			view, err = s.viewMachine(r.Context(), machine)
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
	return &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 * 1024,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			if unix, ok := connection.(*net.UnixConn); ok {
				return context.WithValue(ctx, guestTransportKey{}, unix)
			}
			return ctx
		},
	}
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
	var authorized auth.GuestPrincipal
	var control rt.NativeGuestControl
	var policyWorker workerclient.Client
	var managedPolicy bool
	err := s.Auth.UseGuestGrant(ctx, peer, secret, auth.GuestExec, target, func(tx *sql.Tx, guest auth.GuestPrincipal) error {
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
		authorized = guest
		policyWorker, managedPolicy = lifecycle.PolicyClient(adapter, scope)
		return registry.AppendEvent(ctx, tx, "auth", target, "", map[string]string{"action": "guest_exec_admitted", "grant_id": guest.GrantID}, true)
	})
	if err != nil {
		return err
	}
	check := func(ctx context.Context) error {
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
	if _, err := verify(r); err != nil {
		return err
	}
	if managedPolicy {
		if _, err := s.Engine.ApplyMachinePolicy(ctx, scope, policyWorker); err != nil {
			return err
		}
		hold, err := registry.NewID("guest-exec-")
		if err != nil {
			return err
		}
		execution := lifecycle.WorkerExecution(scope)
		body := map[string]any{"protection_id": hold, "principal": authorized.GrantID, "reason": "guest_exec"}
		var ack struct {
			Revision int64 `json:"revision"`
		}
		if err := policyWorker.Lifecycle(ctx, "AcquireActivityHold", hold+"-open", execution, body, &ack); err != nil {
			return err
		}
		defer func() {
			closeCtx, stop := context.WithTimeout(context.Background(), 4*time.Second)
			defer stop()
			// Recovery can advance an active hold to suspect during the call.
			scope := map[string]any{"protection_id": hold, "principal": authorized.GrantID}
			if err := policyWorker.Lifecycle(closeCtx, "QueryProtection", "", execution, scope, &ack); err == nil {
				scope["expected_revision"] = ack.Revision
				_ = policyWorker.Lifecycle(closeCtx, "ReleaseActivityHold", hold+"-close", execution, scope, nil)
			}
		}()
		if err := check(ctx); err != nil {
			return err
		}
	}
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-call.Done():
				return
			case <-tick.C:
				if err := check(call); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
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
