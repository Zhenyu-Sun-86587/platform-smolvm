package platformapi

import (
	"context"
	"errors"

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
