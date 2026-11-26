package nodegateway

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"time"

	"sandbox-platform/internal/guesttransport"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

const (
	guestRelayListenerLimit = 128
	guestRelayLease         = 30 * time.Second
)

type relayEntry struct {
	scope    rt.GuestRelayScope
	listener *net.UnixListener
	accept   sync.Mutex
	until    time.Time
}

type guestRelays struct {
	mu        sync.Mutex
	listeners map[string]*relayEntry
	closed    bool
}

func (s *guestRelays) acquire(ctx context.Context, path string, scope rt.GuestRelayScope) (*relayEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if s.listeners == nil {
		s.listeners = make(map[string]*relayEntry)
	}
	now := time.Now()
	s.expire(now)
	if entry := s.listeners[path]; entry != nil {
		if entry.scope == scope {
			entry.until = now.Add(guestRelayLease)
			return entry, nil
		}
		if entry.scope.Execution == scope.Execution {
			return nil, rt.ErrConflict
		}
		_ = entry.listener.Close()
		delete(s.listeners, path)
	}
	if len(s.listeners) == guestRelayListenerLimit {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeBusy}
	}
	listener, err := guesttransport.Listen(ctx, path)
	if err != nil {
		return nil, err
	}
	entry := &relayEntry{scope: scope, listener: listener, until: now.Add(guestRelayLease)}
	s.listeners[path] = entry
	return entry, nil
}

func (s *guestRelays) expire(now time.Time) {
	for path, entry := range s.listeners {
		if !now.Before(entry.until) {
			_ = entry.listener.Close()
			delete(s.listeners, path)
		}
	}
}

// RunGuestRelays retires local listeners which this controller no longer
// requests. Active relays retain their own socket and execution checks.
func (s *Server) RunGuestRelays(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.relays.mu.Lock()
			s.relays.closed = true
			for name, entry := range s.relays.listeners {
				_ = entry.listener.Close()
				delete(s.relays.listeners, name)
			}
			s.relays.mu.Unlock()
			return
		case now := <-ticker.C:
			s.relays.mu.Lock()
			s.relays.expire(now)
			s.relays.mu.Unlock()
		}
	}
}

func (s *Server) openGuestRelay(ctx context.Context, route Route, in rt.GuestRelayScope) (net.Conn, error) {
	if route.GuestRelayDir == "" || route.Worker.Socket == "" {
		return nil, rt.ErrUnsupported
	}
	if !nodeproto.ValidID(in.BindingID) || !nodeproto.ValidID(in.Name) || !guesttransport.ValidIncarnation(in.Execution.IncarnationID) {
		return nil, rt.ErrInvalid
	}
	if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
		return nil, err
	}
	control, ok := route.Adapter.(rt.NativeControl)
	if !ok {
		return nil, rt.ErrUnsupported
	}
	observed, err := control.DiscoverExecution(ctx, in.Name)
	if err != nil || observed != in.Execution {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeForbidden}
	}
	path := filepath.Join(route.GuestRelayDir, in.Execution.IncarnationID+".sock")
	entry, err := s.relays.acquire(ctx, path, in)
	if err != nil {
		return nil, err
	}
	if !entry.accept.TryLock() {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeBusy}
	}
	defer entry.accept.Unlock()
	deadline, _ := ctx.Deadline()
	// Keep enough of the opening timeout to send the result after Accept.
	if err := entry.listener.SetDeadline(deadline.Add(-250 * time.Millisecond)); err != nil {
		return nil, err
	}
	verify := route.Worker.VerifyGuestTransport
	if route.verifyGuestTransport != nil {
		verify = route.verifyGuestTransport
	}
	for ctx.Err() == nil {
		conn, err := entry.listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		check := func(ctx context.Context) error {
			if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
				return err
			}
			return verify(ctx, workerExecution(in.Execution), conn)
		}
		if err := check(ctx); err != nil {
			_ = conn.Close()
			continue
		}
		return &verifiedGuestRelay{UnixConn: conn, verify: check}, nil
	}
	return nil, ctx.Err()
}

type verifiedGuestRelay struct {
	*net.UnixConn
	verify func(context.Context) error
}

func (conn *verifiedGuestRelay) scopedContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 500*time.Millisecond)
				err := conn.verify(check)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
