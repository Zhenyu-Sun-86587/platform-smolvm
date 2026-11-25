package platformapi

import (
	"context"
	"errors"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) systemCreateAdmission(ctx context.Context, instanceID string) (func(rt.BundleRoute, string) error, error) {
	route, err := s.DB.GetRuntimeRoute(ctx, instanceID)
	if errors.Is(err, rt.ErrUnsupported) {
		return nil, rt.SystemBootUnavailable(false)
	}
	if err != nil {
		return nil, err
	}
	capability, admission := s.SystemBootCapability(ctx, route)
	if admission == nil && !capability.Supported {
		admission = &rt.CapabilityError{Capability: rt.SystemBootCapability, Status: capability}
	}
	// Check the exact accepted route without I/O inside the writer transaction.
	// The registry returns a matching idempotent replay before consulting this preflight.
	return func(accepted rt.BundleRoute, _ string) error {
		if accepted != route {
			return rt.SystemBootUnavailable(false)
		}
		return admission
	}, nil
}

func (s *Server) resolveSystemNetwork(input lifecycle.Create, accept *registry.AcceptRequest) {
	admit := accept.AdmitRuntime
	var topologyError error
	for _, route := range s.Config.Runtime.Custom {
		if route.InstanceID != input.RuntimeInstanceID {
			continue
		}
		network, err := route.ResolvePrivateNetwork(input.Network)
		if err == nil && network != nil {
			input.Network = network
			accept.ResolvedCreateRequest = lifecycle.JSON(input)
		}
		if err != nil {
			topologyError = errors.Join(registry.ErrInvalid, err)
		}
		accept.AdmitRuntime = func(accepted rt.BundleRoute, kind string) error {
			if accepted != route.Key() {
				return rt.SystemBootUnavailable(false)
			}
			if topologyError != nil {
				return topologyError
			}
			return admit(accepted, kind)
		}
		return
	}
	if input.Network != nil {
		accept.AdmitRuntime = func(rt.BundleRoute, string) error { return registry.ErrInvalid }
	}
}
