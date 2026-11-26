package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

func (s *Server) systemCreateAdmission(ctx context.Context, instanceID string) (func(rt.BundleRoute, string) error, error) {
	route, err := s.DB.GetRuntimeRoute(ctx, instanceID)
	if errors.Is(err, rt.ErrUnsupported) {
		return func(rt.BundleRoute, string) error { return rt.SystemBootUnavailable(false) }, nil
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

// Resolve operator defaults under the registry writer transaction. The
// idempotency check precedes this callback, including on a recovered request.
func (s *Server) createDefaults(input lifecycle.Create, boot templates.Spec, useDefaultBoot, managed, useDefaultNetwork bool) func(context.Context, *sql.Tx) (json.RawMessage, json.RawMessage, error) {
	return func(ctx context.Context, tx *sql.Tx) (json.RawMessage, json.RawMessage, error) {
		defaults, err := s.readSettings(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		if useDefaultBoot && defaults.BootMode != boot.BootMode {
			return nil, nil, errors.Join(registry.ErrInvalid, errors.New("the selected template cannot use the default boot mode"))
		}
		resolved := input
		resolved.BootMode = boot.BootMode
		idle := lifecycle.IdlePolicy{}
		if boot.BootMode == rt.BootSystem {
			if useDefaultNetwork || input.Network != nil {
				resolved.Network, err = s.systemNetwork(input, defaults)
				if err != nil {
					return nil, nil, errors.Join(registry.ErrInvalid, err)
				}
			}
		} else {
			idle = lifecycle.IdlePolicy{Enabled: managed, Timeout: defaults.IdleSeconds}
		}
		return lifecycle.JSON(resolved), lifecycle.JSON(idle), nil
	}
}

func (s *Server) systemNetwork(input lifecycle.Create, defaults settings) (*rt.LaunchNetwork, error) {
	if input.Placement != nil || s.remoteRuntime(input.RuntimeInstanceID) {
		if input.Network == nil && defaults.NetworkID == "" {
			return nil, nil
		}
		network := rt.LaunchNetwork{NetworkID: defaults.NetworkID, Mode: defaults.NetworkMode}
		if input.Network != nil {
			if input.Network.NetworkID != "" {
				network.NetworkID = input.Network.NetworkID
			}
			if input.Network.Mode != "" {
				network.Mode = input.Network.Mode
			}
		}
		return &network, nil
	}
	for _, route := range s.Config.Runtime.Custom {
		if route.InstanceID != input.RuntimeInstanceID {
			continue
		}
		if defaults.NetworkID != "" {
			route.DefaultNetworkID = defaults.NetworkID
		}
		selection := input.Network
		if defaults.NetworkMode != "" && (selection != nil || route.DefaultNetworkID != "") {
			resolved := rt.LaunchNetwork{Mode: defaults.NetworkMode}
			if selection != nil {
				resolved.NetworkID = selection.NetworkID
				if selection.Mode != "" {
					resolved.Mode = selection.Mode
				}
			}
			selection = &resolved
		}
		return route.ResolvePrivateNetwork(selection)
	}
	if input.Network != nil {
		return nil, registry.ErrInvalid
	}
	return nil, nil
}
