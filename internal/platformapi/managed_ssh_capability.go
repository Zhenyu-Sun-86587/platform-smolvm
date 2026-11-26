package platformapi

import (
	"encoding/json"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

const managedSSHAccessCapability = "access.managed_ssh"

type managedSSHConstraints struct {
	ManageAllowed bool   `json:"manage_allowed"`
	Path          string `json:"path"`
	HostKeyStatus string `json:"host_key_status"`
	Reachability  string `json:"reachability"`
	AgentExec     string `json:"agent_exec_stream"`
}

type managedSSHView struct {
	capability  rt.Capability
	constraints managedSSHConstraints
}

func (v *machineView) projectManagedSSHPrincipal(p auth.Principal) {
	ssh := v.managedSSH
	ssh.constraints.ManageAllowed = p.Allows(auth.ActionManage, v.ID)
	ssh.capability.Constraints = lifecycle.JSON(ssh.constraints)
	v.Capabilities[managedSSHAccessCapability] = ssh.capability
}

func (s *Server) managedSSHView(v *machineView, profile bool, verifiedKey bool) managedSSHView {
	capability := s.managedSSHSupport(v, profile)
	constraints := managedSSHConstraints{Path: "unknown", HostKeyStatus: "unknown", Reachability: "unknown", AgentExec: "unknown"}
	if capability.Supported {
		constraints.Path = "configured"
		b := v.Binding
		if b.ObservedAt != nil && time.Since(*b.ObservedAt) <= time.Duration(s.Config.Lifecycle.StatusStale)*time.Second {
			switch v.PowerState {
			case registry.PowerRunning, registry.PowerStandby:
				constraints.HostKeyStatus = "unavailable"
				if verifiedKey {
					constraints.HostKeyStatus = "observed"
				}
				var evidence struct {
					Exec *rt.GuestExecStreamEvidence `json:"exec_stream"`
				}
				if json.Unmarshal(b.GuestManagementEvidence, &evidence) == nil && evidence.Exec != nil && evidence.Exec.Execution == b.ExecutionRef() {
					if evidence.Exec.Supported {
						constraints.AgentExec = "observed"
					} else {
						constraints.AgentExec = "unavailable"
						capability = rt.Capability{Status: "unsupported", ReasonCode: "MANAGED_SSH_AGENT_REQUIRED"}
					}
				}
			case registry.PowerStopped, registry.PowerHibernated:
				constraints.HostKeyStatus = "unavailable"
			}
		}
	}
	if capability.Status == "unsupported" {
		constraints.Path = "unavailable"
		constraints.Reachability = "unavailable"
	}
	return managedSSHView{capability: capability, constraints: constraints}
}

func (s *Server) managedSSHSupport(v *machineView, profile bool) rt.Capability {
	b := v.Binding
	if !profile || v.DeletedAt != nil || b != nil && b.RuntimeInstanceID == "" && b.RuntimeBundleID == "" {
		return rt.Capability{Status: "unsupported", ReasonCode: "MANAGED_SSH_PROFILE_REQUIRED"}
	}
	if b == nil || b.Status != "current" || b.Generation != v.BindingGeneration ||
		b.RuntimeInstanceID == "" || b.RuntimeBundleID == "" || b.RuntimeInstanceObjectID == "" {
		return rt.Capability{Status: "unknown", ReasonCode: "MANAGED_SSH_BINDING_UNVERIFIED"}
	}
	adapter, err := s.ForBinding(*b)
	if err != nil {
		return rt.Capability{Status: "unknown", ReasonCode: "MANAGED_SSH_WORKER_UNVERIFIED"}
	}
	_, native := adapter.(rt.NativeGuestControl)
	_, supported := lifecycle.PolicyClient(adapter, *b)
	if !native || !supported {
		return rt.Capability{Status: "unsupported", ReasonCode: "MANAGED_SSH_WORKER_REQUIRED"}
	}
	return rt.Capability{Status: "supported", Supported: true, ContractVersion: 1}
}
