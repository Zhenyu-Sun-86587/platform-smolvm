package nodeclient

import (
	"context"
	"net"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

func (a *Adapter) GuestRelaySupported() bool {
	a.client.mu.RLock()
	defer a.client.mu.RUnlock()
	return a.client.routeInfo[a.scope].GuestRelay
}

func (a *Adapter) AcceptGuestRelay(ctx context.Context, in rt.GuestRelayScope) (net.Conn, error) {
	return openStream(ctx, a, nodeproto.GuestRelayStreamPath, in)
}

func (a *Adapter) AuthorizeGuestRelay(ctx context.Context) error {
	return a.client.authorizeRequest(ctx)
}

var _ rt.GuestRelayBridge = (*Adapter)(nil)
