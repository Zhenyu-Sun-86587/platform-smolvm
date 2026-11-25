package platformapi

import (
	"context"
	"encoding/json"
	"time"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

type machineHealth struct {
	VM              string          `json:"vm"`
	GuestManagement string          `json:"guest_management"`
	Workload        string          `json:"workload"`
	WorkloadError   json.RawMessage `json:"workload_error,omitempty"`
	NetworkIdentity string          `json:"network_identity"`
	Services        string          `json:"services"`
}

type machineView struct {
	PowerState registry.PowerState `json:"power_state"`
	Health     machineHealth       `json:"health"`
	BootMode   rt.BootMode         `json:"boot_mode"`
	registry.Machine
	Binding      *registry.Binding        `json:"binding,omitempty"`
	RuntimeName  string                   `json:"runtime_name,omitempty"`
	Resources    json.RawMessage          `json:"resources,omitempty"`
	GuestStatus  string                   `json:"guest_management_status"`
	Blocked      []string                 `json:"blocked_reasons"`
	Capabilities map[string]rt.Capability `json:"capabilities"`
}

func (s *Server) viewMachine(ctx context.Context, m registry.Machine) (machineView, error) {
	v := machineView{Machine: m, PowerState: registry.PowerUnknown, Health: machineHealth{VM: "unknown", GuestManagement: "unknown", Workload: "unknown", NetworkIdentity: "unknown", Services: "unknown"}, GuestStatus: "unknown", Blocked: []string{}, Capabilities: map[string]rt.Capability{}}
	var spec struct {
		Resources json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return v, err
	}
	v.Resources = spec.Resources
	t, err := s.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil {
		return v, err
	}
	boot, err := templates.ReadSpec(t)
	if err != nil {
		return v, err
	}
	v.BootMode = boot.BootMode
	if len(t.Capabilities) > 0 {
		var raw map[string]json.RawMessage
		if err = json.Unmarshal(t.Capabilities, &raw); err != nil {
			return v, err
		}
		for name, decl := range raw {
			var c rt.Capability
			if len(decl) > 0 && decl[0] == '{' && json.Unmarshal(decl, &c) == nil {
				v.Capabilities[name] = rt.TemplateCapability(c)
			}
		}
	}
	official := m.CurrentBinding == nil || (m.CurrentBinding.RuntimeBundleID == "" && m.CurrentBinding.RuntimeInstanceID == "")
	if !official {
		// Template evidence from the legacy lane does not qualify a new Bundle.
		for k := range v.Capabilities {
			v.Capabilities[k] = rt.Capability{Status: "unknown", ReasonCode: "CAPABILITY_NOT_VERIFIED"}
		}
	}
	for k, c := range rt.FullStateCapabilities(official) {
		v.Capabilities[k] = c
	}
	if m.CurrentBinding != nil {
		b := m.CurrentBinding
		if len(b.Health) > 0 {
			if err := json.Unmarshal(b.Health, &v.Health); err != nil {
				return v, err
			}
		}
		v.PowerState = b.PowerState
		if official {
			v.PowerState = officialPowerState(*b)
			v.Health.VM = m.ObservedState
			v.Health.GuestManagement = b.GuestManagementStatus
		}
		v.Binding = m.CurrentBinding
		v.RuntimeName = m.CurrentBinding.RuntimeName
		v.GuestStatus = m.CurrentBinding.GuestManagementStatus
		if !official {
			for _, kind := range []string{rt.Standby, rt.Wake} {
				v.Capabilities[rt.NativePowerCapability(kind)] = s.NativePowerCapability(*b, kind)
			}
			if boot.BootMode == rt.BootSystem && boot.ManagedSSH {
				key, err := s.VerifiedNativeSSHKey(ctx, *b, lifecycle.SystemSSHPort)
				if err != nil {
					return v, err
				}
				if key == "" && len(b.GuestManagementEvidence) > 0 {
					var evidence map[string]json.RawMessage
					if err := json.Unmarshal(b.GuestManagementEvidence, &evidence); err != nil {
						return v, err
					}
					if evidence != nil && evidence["ssh_ready"] != nil {
						delete(evidence, "ssh_host_key")
						evidence["ssh_ready"] = json.RawMessage("false")
						copy := *b
						copy.GuestManagementEvidence = lifecycle.JSON(evidence)
						v.Binding = &copy
					}
				}
			}
		}
	}
	if m.DeletedAt == nil && m.CurrentBinding != nil && (m.ObservedAt == nil || time.Since(*m.ObservedAt) > time.Duration(s.Config.Lifecycle.StatusStale)*time.Second) {
		v.Availability = "unknown"
		v.PowerState = registry.PowerUnknown
		v.Health.VM = "unknown"
		v.Blocked = append(v.Blocked, "observation_stale")
	}
	if m.ActiveOperationID != "" {
		v.Blocked = append(v.Blocked, "operation_in_progress")
	}
	if official && v.GuestStatus != "ready" && m.ObservedState == "running" {
		v.Blocked = append(v.Blocked, "guest_management_"+v.GuestStatus)
		for _, k := range []string{"persistent_task", "clean_checkpoint", "independent_clone", "active_recovery", "automatic_idle"} {
			v.Capabilities[k] = rt.Capability{Status: "unknown", Supported: false, ReasonCode: "GUEST_MANAGEMENT_UNAVAILABLE", Message: "Guest Task Manager handshake required"}
		}
	}
	return v, nil
}

func officialPowerState(b registry.Binding) registry.PowerState {
	switch b.ObservedState {
	case "running":
		return registry.PowerRunning
	case "created", "stopped":
		return registry.PowerStopped
	case "paused":
		var reason string
		if len(b.StateReason) > 0 && json.Unmarshal(b.StateReason, &reason) != nil {
			return registry.PowerUnknown
		}
		observation := rt.Observation{StateReason: reason}
		if !observation.PressurePaused() && b.PauseStatus == "active" && b.RuntimePauseID != "" {
			return registry.PowerHibernated
		}
	}
	return registry.PowerUnknown
}
