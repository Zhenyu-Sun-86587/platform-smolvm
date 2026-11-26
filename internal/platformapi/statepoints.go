package platformapi

import (
	"context"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) requireStatePointProfile(ctx context.Context, binding registry.Binding, kind string) error {
	if binding.RuntimeInstanceID == "" || binding.RuntimeBundleID == "" {
		return rt.FullStateUnavailable(kind, true)
	}
	var legacyProfile, authorizedScope bool
	if err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT COALESCE(json_extract(manifest_json,'$.contracts.managed_statepoint'),0)=1,
		COALESCE(json_extract(manifest_json,'$.contracts.managed_authorized_scope'),0)=1
		FROM runtime_bundles WHERE id=?`, binding.RuntimeBundleID).Scan(&legacyProfile, &authorizedScope); err != nil {
		return err
	}
	if authorizedScope {
		// The scoped release assigns each operation its own measured capability.
		// The live adapter checks that evidence against this Bundle and its Worker.
		capability, err := s.StatePointCapability(ctx, binding, kind)
		if err != nil {
			return err
		}
		if !capability.Supported {
			return &rt.CapabilityError{Capability: rt.FullStateOperationCapability(kind), Status: capability}
		}
	} else if !legacyProfile {
		return rt.FullStateUnavailable(kind, false)
	}
	return s.AdmitRuntime(binding.BundleRoute(), kind)
}

func (s *Server) machineStatePointProfile(ctx context.Context, id, kind string) error {
	machine, err := s.DB.GetMachine(ctx, id)
	if err != nil {
		return err
	}
	if machine.CurrentBinding == nil {
		return rt.FullStateUnavailable(kind, true)
	}
	return s.requireStatePointProfile(ctx, *machine.CurrentBinding, kind)
}
