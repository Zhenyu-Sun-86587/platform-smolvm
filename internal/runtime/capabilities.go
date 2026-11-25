package runtime

// FullStateCapabilities names operations whose meaning differs from the legacy
// pause, checkpoint and branch APIs. Template declarations alone cannot open them.
func FullStateCapabilities(official bool) map[string]Capability {
	c := Capability{Status: "unknown", ReasonCode: "CAPABILITY_NOT_VERIFIED"}
	if official {
		c = Capability{Status: "unsupported", ReasonCode: "RUNTIME_EXT_REQUIRED"}
	}
	result := make(map[string]Capability)
	for _, key := range []string{"lifecycle.standby", "state.independent_fork", "state.fork_batch", "state.hibernate", "state.clone_ready", "storage.multidisk_capture", "network.native_reidentity", "boot.system_state_restore"} {
		result[key] = c
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
