package lifecycle

import (
	"context"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

// ManagedSystemSSHBinding qualifies the platform-managed System SSH profile
// without waking the machine. The tunnel separately acquires an access hold.
func (e *Engine) ManagedSystemSSHBinding(ctx context.Context, mid string) (registry.Binding, error) {
	machine, err := e.DB.GetMachine(ctx, mid)
	if err != nil {
		return registry.Binding{}, err
	}
	if machine.DeletedAt != nil {
		return registry.Binding{}, registry.ErrNotFound
	}
	template, err := e.DB.GetTemplate(ctx, machine.TemplateID)
	if err != nil {
		return registry.Binding{}, err
	}
	profile, err := templates.ReadSpec(template)
	if err != nil {
		return registry.Binding{}, err
	}
	if profile.BootMode != rt.BootSystem || !profile.ManagedSSH {
		return registry.Binding{}, rt.ErrUnsupported
	}
	binding := machine.CurrentBinding
	if binding == nil || binding.Status != "current" || binding.RuntimeInstanceObjectID == "" {
		return registry.Binding{}, registry.ErrPowerState
	}
	if binding.RuntimeBundleID == "" {
		return registry.Binding{}, rt.ErrUnsupported
	}
	adapter, err := e.ForBinding(*binding)
	if err != nil {
		return registry.Binding{}, err
	}
	if _, ok := adapter.(rt.NativeGuestControl); !ok {
		return registry.Binding{}, rt.ErrUnsupported
	}
	return *binding, nil
}
