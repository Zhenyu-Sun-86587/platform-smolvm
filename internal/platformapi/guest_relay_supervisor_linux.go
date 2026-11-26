package platformapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
)

var relayIncarnation = regexp.MustCompile(`^vmm-[a-f0-9]{32}$`)

// StartGuestRelays publishes host-owned, execution-specific Unix listeners
// reached through the managed guest's vsock. Each request still checks the
// accepted socket and current Worker execution before considering a token.
func (s *Server) StartGuestRelays(ctx context.Context) error {
	routes := map[rt.BundleRoute]customsmolvm.Route{}
	for _, route := range s.Config.Runtime.Custom {
		if route.GuestRelayDir == "" {
			continue
		}
		err := os.Mkdir(route.GuestRelayDir, 0o751)
		if err == nil {
			// The service umask is private while VMM UIDs need directory search.
			if err := os.Chmod(route.GuestRelayDir, 0o751); err != nil {
				return err
			}
		} else if !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(route.GuestRelayDir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("Guest relay directory must belong to platformd and exclude other writers")
		}
		routes[route.Key().Key()] = route
	}
	if len(routes) > 0 {
		go s.superviseGuestRelays(ctx, routes)
	}
	return nil
}

func (s *Server) superviseGuestRelays(ctx context.Context, routes map[rt.BundleRoute]customsmolvm.Route) {
	active := map[string]*http.Server{}
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
		needed := map[string]bool{}
		for _, m := range machines {
			b := m.CurrentBinding
			if b == nil || m.DeletedAt != nil || b.PowerState != registry.PowerRunning || !relayIncarnation.MatchString(b.IncarnationID) {
				continue
			}
			route, ok := routes[b.BundleRoute().Key()]
			if !ok {
				continue
			}
			path := filepath.Join(route.GuestRelayDir, b.IncarnationID+".sock")
			needed[path] = true
			if active[path] != nil {
				continue
			}
			listener, err := listenGuestRelay(ctx, path)
			if err != nil {
				slog.Warn("Guest relay listener unavailable", "machine_id", m.ID, "error", err)
				continue
			}
			server := s.GuestRelay(route.WorkerSocket, *b)
			active[path] = server
			go func() { _ = server.Serve(listener) }()
		}
		for path, server := range active {
			if !needed[path] {
				_ = server.Close()
				delete(active, path)
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

func listenGuestRelay(ctx context.Context, path string) (*net.UnixListener, error) {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("Guest relay path is occupied")
		}
		probe, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			_ = probe.Close()
			return nil, fmt.Errorf("Guest relay is already listening")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Each VMM has a dedicated UID. The Worker authenticates the accepted
	// connection inside our private directory before dispatching any request.
	if err := os.Chmod(path, 0o666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
