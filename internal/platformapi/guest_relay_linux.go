package platformapi

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
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
	peer := auth.AttestedGuestContext{
		MachineID: binding.MachineID, BindingID: binding.ID, RuntimeInstanceID: binding.RuntimeInstanceID,
		RuntimeBundleID: binding.RuntimeBundleID, RuntimeObjectID: execution.InstanceID,
		IncarnationID: execution.IncarnationID, ExecutionEpoch: execution.Epoch,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/machines/{machine}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		connection, _ := r.Context().Value(guestTransportKey{}).(*net.UnixConn)
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || connection == nil || worker.VerifyGuestTransport(r.Context(), execution, connection) != nil {
			fail(w, r, auth.ErrUnauthenticated)
			return
		}
		var view machineView
		target := r.PathValue("machine")
		err := s.Auth.UseGuestGrant(r.Context(), peer, secret, auth.GuestRead, target, func(_ *sql.Tx, _ auth.GuestPrincipal) error {
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
