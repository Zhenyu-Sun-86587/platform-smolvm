package runtime

const (
	Standby         = "standby"
	Hibernate       = "hibernate"
	IndependentFork = "independent_fork"
	ForkBatch       = "fork_batch"
	VMSnapshot      = "vm_snapshot"
	VMRestore       = "vm_restore"
	VMRollback      = "vm_rollback"
)

func FullStateOperationCapability(kind string) string {
	switch kind {
	case Standby:
		return NativePowerCapability(kind)
	case Hibernate:
		return "state.hibernate"
	case IndependentFork:
		return "state.independent_fork"
	case ForkBatch:
		return "state.fork_batch"
	case VMSnapshot:
		return "state.checkpoint"
	case VMRestore:
		return "state.restore"
	case VMRollback:
		return "state.rollback"
	default:
		return ""
	}
}

// These operations take a new full-state cut of the source execution.
func CapturesLiveState(kind string) bool {
	return kind == VMSnapshot || kind == Hibernate || IsNativeFork(kind)
}

func LiveGrowthCaptureStatus() Capability {
	return Capability{Status: "unsupported", ReasonCode: "STATE_READER_TOPOLOGY_UNSUPPORTED",
		Message: "the current StatePoint reader cannot preserve the live compute growth topology"}
}

// CapabilityError records the missing admission evidence before any effect.
type CapabilityError struct {
	Capability string
	Status     Capability
}

func (e *CapabilityError) Error() string {
	return e.Capability + " is " + e.Status.Status + " for this runtime"
}

func FullStateUnavailable(kind string, official bool) error {
	return &CapabilityError{Capability: FullStateOperationCapability(kind), Status: fullStateCapability(official)}
}

func fullStateCapability(official bool) Capability {
	if official {
		return Capability{Status: "unsupported", ReasonCode: "RUNTIME_EXT_REQUIRED"}
	}
	return Capability{Status: "unknown", ReasonCode: "CAPABILITY_NOT_VERIFIED"}
}

// FullStateCapabilities names operations whose meaning differs from the legacy
// pause, checkpoint and branch APIs. Template declarations alone cannot open them.
func FullStateCapabilities(official bool) map[string]Capability {
	c := fullStateCapability(official)
	result := map[string]Capability{
		LiveComputeGrowthCapability: c,
		NativePowerCapability(Wake): c,
		SystemBootCapability:        c,
		KernelCowLaunchCapability:   c,
		"state.live_fork":           c,
		"state.clone_ready":         c,
		"storage.multidisk_capture": c,
		"network.native_reidentity": c,
		"boot.system_state_restore": c,
	}
	if !official {
		result[NativePowerCapability("stop")] = c
	}
	for _, kind := range []string{Standby, Hibernate, IndependentFork, ForkBatch, VMSnapshot, VMRestore, VMRollback} {
		result[FullStateOperationCapability(kind)] = c
	}
	return result
}

// TemplateCapability accepts Phase 1.5 declarations with only the original
// boolean. An explicit new status must agree with that compatibility field.
func TemplateCapability(c Capability) Capability {
	if c.Status == "" {
		switch {
		case c.Supported:
			c.Status = "supported"
		case c.ReasonCode == "CAPABILITY_NOT_VERIFIED" || c.ReasonCode == "":
			c.Status = "unknown"
		default:
			c.Status = "unsupported"
		}
	}
	switch c.Status {
	case "supported":
		if !c.Supported {
			c.Status = "unknown"
		}
	case "unsupported", "unknown":
		c.Supported = false
	default:
		c.Status = "unknown"
		c.Supported = false
	}
	return c
}
