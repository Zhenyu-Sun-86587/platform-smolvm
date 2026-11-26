package platformapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"sandbox-platform/internal/guesttransport"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
)

// StartGuestRelays serves the Guest-grant API on Worker-verified connections
// from execution-specific host sockets or an enrolled Node's reverse stream.
func (s *Server) StartGuestRelays(ctx context.Context) error {
	routes := map[rt.BundleRoute]customsmolvm.Route{}
	for _, route := range s.Config.Runtime.Custom {
		if route.GuestRelayDir == "" {
			continue
		}
		if err := guesttransport.EnsureDirectory(route.GuestRelayDir); err != nil {
			return err
		}
		routes[route.Key().Key()] = route
	}
	workers, err := s.DB.ListWorkers(ctx)
	if err != nil {
		return err
	}
	if len(routes) > 0 || len(workers) > 1 {
		go s.superviseGuestRelays(ctx, routes)
	}
	return nil
}

type guestRelayID struct {
	Worker, Binding, Incarnation string
	Epoch                        int64
}

func (s *Server) superviseGuestRelays(ctx context.Context, routes map[rt.BundleRoute]customsmolvm.Route) {
	active := map[guestRelayID]*http.Server{}
	defer func() {
		for _, server := range active {
			_ = server.Close()
		}
	}()
	reconcile := func() {
		machines, err := s.DB.ListMachines(ctx)
		if err != nil {
			return
		}
		needed := make(map[guestRelayID]bool)
		for _, m := range machines {
			b := m.CurrentBinding
			if b == nil || m.DeletedAt != nil || b.PowerState != registry.PowerRunning || !guesttransport.ValidIncarnation(b.IncarnationID) {
				continue
			}
			id := guestRelayID{Worker: b.WorkerID, Binding: b.ID, Incarnation: b.IncarnationID, Epoch: b.ExecutionEpoch}
			if b.BundleRoute().Owner() == rt.LocalWorkerID {
				route, ok := routes[b.BundleRoute().Key()]
				if !ok {
					continue
				}
				needed[id] = true
				if active[id] == nil {
					path := filepath.Join(route.GuestRelayDir, b.IncarnationID+".sock")
					listener, err := guesttransport.Listen(ctx, path)
					if err != nil {
						slog.Warn("Guest relay listener unavailable", "machine_id", m.ID, "error", err)
						continue
					}
					active[id] = runGuestRelay(s.GuestRelay(route.WorkerSocket, *b), listener)
				}
				continue
			}
			adapter, err := s.ForBinding(*b)
			bridge, ok := adapter.(rt.GuestRelayBridge)
			if err != nil || !ok || !bridge.GuestRelaySupported() {
				continue
			}
			needed[id] = true
			if active[id] == nil {
				active[id] = s.startRemoteGuestRelay(ctx, *b, bridge)
			}
		}
		for id, server := range active {
			if !needed[id] {
				_ = server.Close()
				delete(active, id)
			}
		}
	}
	reconcile()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			reconcile()
		}
	}
}

func runGuestRelay(server *http.Server, listener net.Listener) *http.Server {
	go func() { _ = server.Serve(listener) }()
	return server
}
