package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
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

func (s *Server) privateNetworks(ctx context.Context) ([]privateNetwork, error) {
	items := []privateNetwork{}
	for _, route := range s.Config.Runtime.Custom {
		for _, network := range route.PrivateNetworks {
			items = append(items, privateNetwork{ID: network.ID, RuntimeInstanceID: route.InstanceID, CIDR: network.CIDR})
		}
	}
	for _, n := range s.Config.Nodes {
		sample, _, err := s.DB.GetWorkerSample(ctx, n.ID)
		if errors.Is(err, registry.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, profile := range sample.Profiles {
			for _, network := range profile.Networks {
				items = append(items, privateNetwork{ID: network.ID, RuntimeInstanceID: profile.RuntimeInstanceID, CIDR: network.CIDR})
			}
		}
	}
	return items, nil
}

func (s *Server) hasPrivateNetwork(ctx context.Context, id string) (bool, error) {
	networks, err := s.privateNetworks(ctx)
	return slices.ContainsFunc(networks, func(n privateNetwork) bool { return n.ID == id }), err
}

func (s *Server) networkRoutes() {
	s.privatePacketWakeRoutes()
	s.route("GET /v1/networks", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		items, err := s.privateNetworks(r.Context())
		if err != nil {
			return err
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
