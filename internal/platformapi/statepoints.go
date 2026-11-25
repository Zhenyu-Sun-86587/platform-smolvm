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
	var advertised bool
	if err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT COALESCE(json_extract(manifest_json,'$.contracts.managed_statepoint'),0)=1
		FROM runtime_bundles WHERE id=?`, binding.RuntimeBundleID).Scan(&advertised); err != nil {
		return err
	}
	if !advertised {
		return rt.FullStateUnavailable(kind, false)
	}
	return s.AdmitRuntime(rt.BundleRoute{InstanceID: binding.RuntimeInstanceID, BundleID: binding.RuntimeBundleID}, kind)
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
