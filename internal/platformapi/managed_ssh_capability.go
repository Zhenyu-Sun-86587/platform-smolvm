package platformapi

import (
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
	GuestPort     uint16 `json:"guest_port,omitempty"`
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

func (s *Server) managedSSHView(v *machineView, profile bool, network *rt.LaunchNetwork, verifiedKey bool) managedSSHView {
	capability := s.managedSSHSupport(v, profile, network)
	constraints := managedSSHConstraints{Path: "unknown", HostKeyStatus: "unknown", Reachability: "unknown"}
	switch capability.Status {
	case "unsupported":
		constraints.Path = "unavailable"
		constraints.Reachability = "unavailable"
	case "supported":
		// A configured path enables the entry point. The tunnel still checks
		// the live Worker attachment and proves the Guest host key on access.
		constraints.Path = "configured"
		constraints.GuestPort = lifecycle.SystemSSHPort
		observedAt := v.Binding.ObservedAt
		if observedAt != nil && time.Since(*observedAt) <= time.Duration(s.Config.Lifecycle.StatusStale)*time.Second {
			switch v.PowerState {
			case registry.PowerRunning, registry.PowerStandby:
				constraints.HostKeyStatus = "unavailable"
				if verifiedKey {
					constraints.HostKeyStatus = "observed"
				}
			case registry.PowerStopped, registry.PowerHibernated:
				constraints.HostKeyStatus = "unavailable"
			}
		}
	}
	return managedSSHView{capability: capability, constraints: constraints}
}

func (s *Server) managedSSHSupport(v *machineView, profile bool, network *rt.LaunchNetwork) rt.Capability {
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
	worker, supported := lifecycle.PolicyClient(adapter, *b)
	if !native || !supported {
		return rt.Capability{Status: "unsupported", ReasonCode: "MANAGED_SSH_WORKER_REQUIRED"}
	}
	noNetwork := rt.Capability{Status: "unsupported", ReasonCode: "MANAGED_SSH_PRIVATE_NETWORK_REQUIRED"}
	if network == nil || network.NetworkID == "" {
		return noNetwork
	}
	key := rt.BundleRoute{InstanceID: b.RuntimeInstanceID, BundleID: b.RuntimeBundleID}
	for _, route := range s.Config.Runtime.Custom {
		if route.Key() != key || route.WorkerSocket != worker.Socket {
			continue
		}
		resolved, err := route.ResolvePrivateNetwork(network)
		if err != nil || resolved == nil ||
			network.RouteDomainID != "" && network.RouteDomainID != resolved.RouteDomainID ||
			network.CIDR != "" && network.CIDR != resolved.CIDR {
			return noNetwork
		}
		return rt.Capability{Status: "supported", Supported: true, ContractVersion: 1}
	}
	return rt.Capability{Status: "unknown", ReasonCode: "MANAGED_SSH_WORKER_UNVERIFIED"}
}
