package registry

import (
	"fmt"

	rt "sandbox-platform/internal/runtime"
)

// RequirePowerState fences explicit saved-state wake and cold boot. Existing
// callers can omit the guard and retain the normal start/wake behavior.
func RequirePowerState(binding *Binding, expected PowerState) error {
	if binding == nil || binding.ID == "" {
		return ErrGeneration
	}
	actual := binding.PowerState
	if binding.RuntimeBundleID == "" && binding.RuntimeInstanceID == "" {
		if expected != PowerStopped {
			capability := rt.NativePowerCapability(rt.Wake)
			if expected == PowerHibernated {
				capability = rt.FullStateOperationCapability(rt.VMRestore)
			}
			return &rt.CapabilityError{Capability: capability, Status: rt.FullStateCapabilities(true)[capability]}
		}
		switch binding.ObservedState {
		case "created", "stopped":
			actual = PowerStopped
		case "running":
			actual = PowerRunning
		default:
			actual = PowerUnknown
		}
	}
	if actual != expected {
		return fmt.Errorf("%w: expected %s, observed %s", ErrPowerState, expected, actual)
	}
	return nil
}
