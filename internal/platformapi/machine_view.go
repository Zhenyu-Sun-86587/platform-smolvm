package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	WebURL            string                      `json:"web_url,omitempty"`
	PowerState        registry.PowerState         `json:"power_state"`
	Health            machineHealth               `json:"health"`
	BootMode          rt.BootMode                 `json:"boot_mode"`
	MemoryForkProfile string                      `json:"memory_fork_profile,omitempty"`
	LiveComputeGrowth bool                        `json:"live_compute_growth,omitempty"`
	DesktopProfile    string                      `json:"desktop_profile,omitempty"`
	Network           *rt.LaunchNetwork           `json:"network,omitempty"`
	Lifecycle         *registry.LifecycleSettings `json:"lifecycle_policy,omitempty"`
	registry.Machine
	Binding      *registry.Binding        `json:"binding,omitempty"`
	RuntimeName  string                   `json:"runtime_name,omitempty"`
	Resources    json.RawMessage          `json:"resources,omitempty"`
	GuestStatus  string                   `json:"guest_management_status"`
	TaskMetadata string                   `json:"task_metadata_status,omitempty"`
	Blocked      []string                 `json:"blocked_reasons"`
	Capabilities map[string]rt.Capability `json:"capabilities"`
	managedSSH   managedSSHView
}

func (s *Server) viewMachine(ctx context.Context, m registry.Machine, refreshPolicy bool) (machineView, error) {
	v := machineView{Machine: m, PowerState: registry.PowerUnknown, Health: machineHealth{VM: "unknown", GuestManagement: "unknown", Workload: "unknown", NetworkIdentity: "unknown", Services: "unknown"}, GuestStatus: "unknown", Blocked: []string{}, Capabilities: map[string]rt.Capability{}}
	if host := machineWebHost(m.ID, s.Config.Gateway.AppsDomain); host != "" && m.DeletedAt == nil {
		v.WebURL = "https://" + host
	}
	verifiedSSHKey := false
	var spec struct {
		MemoryForkProfile string            `json:"memory_fork_profile"`
		Resources         json.RawMessage   `json:"resources"`
		Network           *rt.LaunchNetwork `json:"network"`
		LiveComputeGrowth bool              `json:"live_compute_growth"`
	}
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return v, err
	}
	if m.CurrentBinding != nil && m.CurrentBinding.RuntimeBundleID != "" {
		policy, err := s.lifecycleView(ctx, m, refreshPolicy)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return v, err
		}
		if err == nil {
			v.Lifecycle = &policy
		}
	}
	v.Resources = spec.Resources
	v.MemoryForkProfile = spec.MemoryForkProfile
	v.LiveComputeGrowth = spec.LiveComputeGrowth
	if spec.Network != nil {
		v.Network = &rt.LaunchNetwork{NetworkID: spec.Network.NetworkID, Mode: spec.Network.Mode}
	}
	t, err := s.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil {
		return v, err
	}
	boot, err := templates.ReadSpec(t)
	if err != nil {
		return v, err
	}
	v.BootMode = boot.BootMode
	v.DesktopProfile = boot.DesktopProfile
	if len(t.Capabilities) > 0 {
		var raw map[string]json.RawMessage
		if err = json.Unmarshal(t.Capabilities, &raw); err != nil {
			return v, err
		}
		for name, decl := range raw {
			if name == managedSSHAccessCapability {
				continue
			}
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
			v.TaskMetadata = lifecycle.TaskMetadataStatus(*b)
			for _, kind := range []string{rt.Standby, rt.Wake} {
				v.Capabilities[rt.NativePowerCapability(kind)] = s.NativePowerCapability(*b, kind)
			}
			if boot.BootMode == rt.BootSystem {
				v.Capabilities[rt.NativePowerCapability("stop")] = s.NativePowerCapability(*b, "stop")
			}
			for _, kind := range []string{rt.VMSnapshot, rt.Hibernate, rt.VMRestore, rt.VMRollback} {
				if capability, err := s.StatePointCapability(ctx, *b, kind); err == nil {
					v.Capabilities[rt.FullStateOperationCapability(kind)] = capability
				}
			}
			for _, kind := range []string{rt.IndependentFork, rt.ForkBatch} {
				v.Capabilities[rt.FullStateOperationCapability(kind)] = s.NativeForkCapability(*b, kind)
			}
			if boot.BootMode == rt.BootSystem && boot.ManagedSSH {
				key, err := s.VerifiedNativeSSHKey(ctx, *b, lifecycle.SystemSSHPort)
				if err != nil {
					return v, err
				}
				verifiedSSHKey = key != ""
				if !verifiedSSHKey && len(b.GuestManagementEvidence) > 0 {
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
	if !official && v.TaskMetadata != "" {
		if v.TaskMetadata == "complete" && v.PowerState != registry.PowerRunning {
			v.TaskMetadata = "stale"
		}
		if v.TaskMetadata == "complete" && v.Lifecycle != nil && v.Lifecycle.Supported {
			v.Capabilities["persistent_task"] = rt.Capability{Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: "taskd-v1"}
		} else {
			v.Capabilities["persistent_task"] = rt.Capability{Status: "unknown", ReasonCode: "TASK_METADATA_UNAVAILABLE", Message: "Current Guest Task Manager confirmation required"}
		}
	}
	if official && v.GuestStatus != "ready" && m.ObservedState == "running" {
		v.Blocked = append(v.Blocked, "guest_management_"+v.GuestStatus)
		for _, k := range []string{"persistent_task", "clean_checkpoint", "independent_clone", "active_recovery", "automatic_idle"} {
			v.Capabilities[k] = rt.Capability{Status: "unknown", Supported: false, ReasonCode: "GUEST_MANAGEMENT_UNAVAILABLE", Message: "Guest Task Manager handshake required"}
		}
	}
	s.projectFileCapabilities(&v, official)
	if !official && v.LiveComputeGrowth {
		for _, kind := range []string{rt.Hibernate, rt.VMSnapshot, rt.IndependentFork, rt.ForkBatch} {
			v.Capabilities[rt.FullStateOperationCapability(kind)] = rt.LiveGrowthCaptureStatus()
		}
	}
	v.managedSSH = s.managedSSHView(&v, boot.BootMode == rt.BootSystem && boot.ManagedSSH, verifiedSSHKey)
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
