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
