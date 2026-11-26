package runtime

const LocalWorkerID = "local-worker"

// BundleRoute identifies one operator-registered adapter for a stored runtime.
type BundleRoute struct {
	WorkerID        string
	InstanceID      string
	BundleID        string
	LocalInstanceID string
}

func (route BundleRoute) Owner() string {
	if route.WorkerID == "" {
		return LocalWorkerID
	}
	return route.WorkerID
}

func (route BundleRoute) Legacy() bool {
	return route.InstanceID == "" && route.BundleID == "" && route.LocalInstanceID == ""
}

// Key is the central adapter identity. The local Serve ID is checked against
// the registration and is kept separate from the global instance ID.
func (route BundleRoute) Key() BundleRoute {
	return BundleRoute{WorkerID: route.Owner(), InstanceID: route.InstanceID, BundleID: route.BundleID}
}

func (route BundleRoute) LocalID() string {
	if route.LocalInstanceID == "" && route.Owner() == LocalWorkerID {
		return route.InstanceID
	}
	return route.LocalInstanceID
}

// BindingRuntime is the runtime metadata persisted with a VM binding.
type BindingRuntime struct {
	BundleRoute
	ObjectID      string
	IncarnationID string
	Epoch         int64
}

func (binding BindingRuntime) Legacy() bool {
	return binding.BundleRoute.Legacy() && binding.ObjectID == "" && binding.IncarnationID == "" && binding.Epoch == 0
}

type adapterRegistration struct {
	localID string
	adapter Adapter
}

type Resolver struct {
	official Adapter
	bundles  map[BundleRoute]adapterRegistration
}

func NewResolver(official Adapter, bundles map[BundleRoute]Adapter) *Resolver {
	registered := make(map[BundleRoute]adapterRegistration, len(bundles))
	for route, adapter := range bundles {
		key := route.Key()
		if _, ambiguous := registered[key]; ambiguous {
			registered[key] = adapterRegistration{}
			continue
		}
		registered[key] = adapterRegistration{localID: route.LocalID(), adapter: adapter}
	}
	return &Resolver{official: official, bundles: registered}
}

func (r *Resolver) Official() Adapter { return r.official }

// Admit checks whether the adapter implements the native dispatch path for a
// binding operation. A wire capability alone cannot admit a mutation.
func (r *Resolver) Admit(route BundleRoute, kind string) error {
	if kind == VolumeOperation {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(VolumeControl); err == nil && ok && !route.Legacy() {
			return nil
		}
		return ErrUnsupported
	}
	if !route.Legacy() && IsNativeFork(kind) {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(NativeForkControl); err == nil && ok {
			return nil
		}
		return FullStateUnavailable(kind, false)
	}
	if !route.Legacy() && NativePowerCapability(kind) != "" {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if kind == "stop" {
			if stop, ok := adapter.(NativeStopControl); err == nil && ok && stop.NativeStopContract() {
				return nil
			}
			return NativePowerUnavailable(kind)
		}
		if _, ok := adapter.(NativeControl); err == nil && ok {
			if contract, scoped := adapter.(interface{ NativeStandbyContract() bool }); scoped && !contract.NativeStandbyContract() {
				return NativePowerUnavailable(kind)
			}
			return nil
		}
		return NativePowerUnavailable(kind)
	}
	if !route.Legacy() && kind == "start" {
		adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
		if _, ok := adapter.(NativeLauncher); err == nil && ok {
			return nil
		}
	}
	if FullStateOperationCapability(kind) != "" {
		if ManagedStatePoint(kind) && !route.Legacy() {
			adapter, err := r.ForBinding(BindingRuntime{BundleRoute: route})
			if _, ok := adapter.(StatePointControl); err == nil && ok {
				return nil
			}
		}
		return FullStateUnavailable(kind, route.Legacy())
	}
	if route.Legacy() && route.Owner() == LocalWorkerID {
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
		if binding.Owner() != LocalWorkerID {
			return nil, &Error{Code: "NOT_SUPPORTED", Message: "legacy runtime requires the local Worker"}
		}
		return r.official, nil
	}
	if binding.InstanceID == "" || binding.BundleID == "" {
		return nil, &Error{Code: "NOT_SUPPORTED", Message: "binding has incomplete runtime routing metadata"}
	}
	registration := r.bundles[binding.BundleRoute.Key()]
	if registration.adapter == nil || registration.localID == "" ||
		(binding.LocalInstanceID != "" && binding.LocalInstanceID != registration.localID) {
		return nil, &Error{Code: "NOT_SUPPORTED", Message: "binding runtime instance and bundle have no registered lifecycle adapter"}
	}
	return registration.adapter, nil
}
