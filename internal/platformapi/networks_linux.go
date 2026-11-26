package platformapi

import (
	"encoding/json"
	"net/http"
	"sandbox-platform/internal/runtime/workerclient"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type privateNetwork struct {
	ID                string `json:"network_id"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	CIDR              string `json:"cidr"`
}

type privateEndpoint struct {
	NetworkID   string `json:"network_id"`
	Mode        string `json:"mode"`
	State       string `json:"state"`
	ID          string `json:"attachment_id,omitempty"`
	PrivateIPv4 string `json:"private_ipv4,omitempty"`
	GuestIPv4   string `json:"guest_ipv4,omitempty"`
	DNSName     string `json:"dns_name,omitempty"`
}

func (s *Server) hasPrivateNetwork(id string) bool {
	for _, route := range s.Config.Runtime.Custom {
		for _, network := range route.PrivateNetworks {
			if network.ID == id {
				return true
			}
		}
	}
	return false
}

func (s *Server) networkRoutes() {
	s.privatePacketWakeRoutes()
	s.route("GET /v1/networks", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		items := []privateNetwork{}
		for _, route := range s.Config.Runtime.Custom {
			for _, network := range route.PrivateNetworks {
				items = append(items, privateNetwork{ID: network.ID, RuntimeInstanceID: route.InstanceID, CIDR: network.CIDR})
			}
		}
		return list(w, r, items, func(n privateNetwork) string { return n.RuntimeInstanceID + "/" + n.ID })
	})
	s.route("GET /v1/machines/{machine}/network", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize(auth.ActionRead, id); err != nil {
			return err
		}
		ctx := r.Context()
		machine, err := s.DB.GetMachine(ctx, id)
		if err != nil {
			return err
		}
		var spec struct {
			Network *rt.LaunchNetwork `json:"network"`
		}
		if err := json.Unmarshal(machine.Spec, &spec); err != nil {
			return err
		}
		if machine.DeletedAt != nil || spec.Network == nil {
			return registry.ErrNotFound
		}
		endpoint := privateEndpoint{NetworkID: spec.Network.NetworkID, Mode: spec.Network.Mode, State: "unavailable"}
		if binding := machine.CurrentBinding; binding != nil {
			adapter, err := s.ForBinding(*binding)
			if worker := workerclient.ForAdapter(adapter, binding.RuntimeBundleID); err == nil && worker != nil {
				attachment, err := workerServiceAttachment(ctx, worker, *binding)
				if err == nil && (attachment.NetworkID == "" || attachment.NetworkID == spec.Network.NetworkID) && attachment.Lease.Attachment.Mode == endpoint.Mode {
					lease := attachment.Lease
					endpoint.State = "published"
					endpoint.ID = lease.Attachment.ID
					endpoint.PrivateIPv4 = lease.Attachment.Address.String()
					endpoint.GuestIPv4 = lease.Attachment.GuestAddress.String()
					endpoint.DNSName = attachment.DNSName
				}
			}
			if endpoint.State == "unavailable" && binding.PowerState == registry.PowerHibernated {
				if saved, err := s.packetWakeEndpoint(ctx, id); err == nil && saved.endpoint.NetworkID == endpoint.NetworkID {
					endpoint.State = "reserved"
					endpoint.ID = saved.endpoint.AttachmentID
					endpoint.PrivateIPv4 = saved.endpoint.PrivateIPv4
					endpoint.GuestIPv4 = saved.endpoint.GuestIPv4
					endpoint.DNSName = saved.endpoint.DNSName
				}
			}
		}
		return write(w, http.StatusOK, endpoint)
	})
}
