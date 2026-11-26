package customsmolvm

import (
	"context"
	"errors"
	"runtime"
	"slices"

	"sandbox-platform/internal/placement"
)

// PlacementProfile reads Serve's process identity separately from resourced's
// OS boot and broker session. Capabilities come from the verified handshake.
func (a *Adapter) PlacementProfile(ctx context.Context) (placement.RuntimeProfile, error) {
	p := placement.RuntimeProfile{LocalInstanceID: a.route.InstanceID, BundleID: a.route.Bundle, Reader: a.route.Reader}
	endpoint, h, err := a.connect(ctx)
	if err != nil {
		return p, err
	}
	defer endpoint.CloseIdleConnections()
	if err = get(ctx, endpoint, "/readyz", nil); err != nil {
		return p, err
	}
	var observed struct {
		Process string `json:"boot_id"`
	}
	if err = get(ctx, endpoint, "/capacity", &observed); err != nil {
		return p, err
	}
	if observed.Process == "" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return p, errors.New("unqualified Serve platform or missing Serve process identity")
	}
	p.RuntimeProcessID, p.Architecture, p.Hypervisor = observed.Process, "x86_64", "kvm"
	for name, c := range h.Capabilities {
		if c.Supported {
			p.Capabilities = append(p.Capabilities, name)
		}
	}
	slices.Sort(p.Capabilities)
	p.Ready = true
	return p, nil
}
