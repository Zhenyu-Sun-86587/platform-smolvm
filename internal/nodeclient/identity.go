package nodeclient

import (
	"context"

	"sandbox-platform/internal/nodeproto"
)

type reconciliationIdentityKey struct{}

// WithReconciliationIdentity selects the authenticated candidate for discovery.
// Ordinary API requests still use the last centrally approved session.
func WithReconciliationIdentity(ctx context.Context, candidate nodeproto.NodeIdentity) context.Context {
	return context.WithValue(ctx, reconciliationIdentityKey{}, candidate)
}

func allowsDiscovery(path string, input any) bool {
	switch path {
	case nodeproto.SamplePath, nodeproto.ReconcileKnownPath, nodeproto.GatePath,
		nodeproto.WorkerPath + "policy/GetLifecycleStatus":
		return true
	case nodeproto.SetGatePath:
		set, ok := input.(nodeproto.SetGate)
		return ok && set.RestrictOnly && (set.Mode == nodeproto.GateCordoned || set.Mode == nodeproto.GateDraining)
	default:
		return false
	}
}

func (c *Client) requestIdentity(ctx context.Context, path string, input any) (nodeproto.NodeIdentity, func(context.Context) error, error) {
	if path == nodeproto.HandshakePath {
		return nodeproto.NodeIdentity{}, nil, nil
	}
	if err := c.authorizeRequest(ctx); err != nil {
		return nodeproto.NodeIdentity{}, nil, err
	}
	var candidate nodeproto.NodeIdentity
	if allowsDiscovery(path, input) {
		candidate, _ = ctx.Value(reconciliationIdentityKey{}).(nodeproto.NodeIdentity)
	}
	identity, err := c.resolveNodeIdentity(ctx, candidate)
	if err != nil {
		return nodeproto.NodeIdentity{}, nil, err
	}
	if c.authorize == nil && c.selectNode == nil {
		return identity, nil, nil
	}
	check := func(ctx context.Context) error {
		if err := c.authorizeRequest(ctx); err != nil {
			return err
		}
		now, err := c.resolveNodeIdentity(ctx, candidate)
		if err != nil {
			return err
		}
		if now != identity {
			return &nodeproto.Failure{Code: nodeproto.CodeAuthority}
		}
		return nil
	}
	return identity, check, nil
}

func (c *Client) resolveNodeIdentity(ctx context.Context, candidate nodeproto.NodeIdentity) (nodeproto.NodeIdentity, error) {
	if c.selectNode != nil {
		return c.selectNode(ctx, candidate)
	}
	c.mu.RLock()
	identity := c.lastHandshake
	c.mu.RUnlock()
	if !identity.Valid() {
		hello, err := c.Handshake(ctx)
		if err != nil {
			return nodeproto.NodeIdentity{}, err
		}
		return hello.Identity(), nil
	}
	return identity, nil
}
