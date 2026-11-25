package runtime

import "maps"

// BundleRoute identifies one operator-registered adapter for a stored runtime.
type BundleRoute struct {
	InstanceID string
	BundleID   string
}

// BindingRuntime is the runtime metadata persisted with a VM binding.
type BindingRuntime struct {
	BundleRoute
	ObjectID      string
	IncarnationID string
	Epoch         int64
}

func (binding BindingRuntime) Legacy() bool { return binding == (BindingRuntime{}) }

type Resolver struct {
	official Adapter
	bundles  map[BundleRoute]Adapter
}

func NewResolver(official Adapter, bundles map[BundleRoute]Adapter) *Resolver {
	return &Resolver{official: official, bundles: maps.Clone(bundles)}
}

func (r *Resolver) Official() Adapter { return r.official }

// Admit checks whether the adapter implements the native dispatch path for a
// binding operation. A wire capability alone cannot admit a mutation.
func (r *Resolver) Admit(route BundleRoute, kind string) error {
	if kind == VolumeOperation {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(VolumeControl); err == nil && ok && route != (BundleRoute{}) {
			return nil
		}
		return ErrUnsupported
	}
	if route != (BundleRoute{}) && IsNativeFork(kind) {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(NativeForkControl); err == nil && ok {
			return nil
		}
		return FullStateUnavailable(kind, false)
	}
	if route != (BundleRoute{}) && NativePowerCapability(kind) != "" {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(NativeControl); err == nil && ok {
			return nil
		}
		return NativePowerUnavailable(kind)
	}
	if route != (BundleRoute{}) && kind == "start" {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(NativeLauncher); err == nil && ok {
			return nil
		}
	}
	if FullStateOperationCapability(kind) != "" {
		if ManagedStatePoint(kind) && route != (BundleRoute{}) {
			adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
			if _, ok := adapter.(StatePointControl); err == nil && ok {
				return nil
			}
		}
		return FullStateUnavailable(kind, route == (BundleRoute{}))
	}
	if route == (BundleRoute{}) {
		return nil
	}
	adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
	if err != nil {
		return err
	}
	if admitting, ok := adapter.(interface{ Admit(string) error }); ok {
		return admitting.Admit(kind)
	}
	return &Error{Code: "NOT_SUPPORTED", Message: "runtime operation requires native dispatch and Worker admission"}
}

// ForBinding uses the legacy adapter for bindings with no runtime metadata.
// A bundle-bound VM requires an exact route, even when another instance uses
// the same bundle.
func (r *Resolver) ForBinding(binding BindingRuntime) (Adapter, error) {
	if binding.Legacy() {
		return r.official, nil
	}
	if binding.InstanceID == "" || binding.BundleID == "" {
		return nil, &Error{Code: "NOT_SUPPORTED", Message: "binding has incomplete runtime routing metadata"}
	}
	adapter, ok := r.bundles[binding.BundleRoute]
	if !ok || adapter == nil {
		return nil, &Error{Code: "NOT_SUPPORTED", Message: "binding runtime instance and bundle have no registered lifecycle adapter"}
	}
	return adapter, nil
}
