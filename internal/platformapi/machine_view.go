package platformapi

import (
	"context"
	"encoding/json"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type machineView struct {
	registry.Machine
	Binding      *registry.Binding        `json:"binding,omitempty"`
	RuntimeName  string                   `json:"runtime_name,omitempty"`
	Resources    json.RawMessage          `json:"resources,omitempty"`
	GuestStatus  string                   `json:"guest_management_status"`
	Blocked      []string                 `json:"blocked_reasons"`
	Capabilities map[string]rt.Capability `json:"capabilities"`
}

func (s *Server) viewMachine(ctx context.Context, m registry.Machine) (machineView, error) {
	v := machineView{Machine: m, GuestStatus: "unknown", Blocked: []string{}, Capabilities: map[string]rt.Capability{}}
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
	if len(t.Capabilities) > 0 {
		var raw map[string]json.RawMessage
		if err = json.Unmarshal(t.Capabilities, &raw); err != nil {
			return v, err
		}
		for name, decl := range raw {
			var c rt.Capability
			if len(decl) > 0 && decl[0] == '{' && json.Unmarshal(decl, &c) == nil {
				v.Capabilities[name] = c
			}
		}
	}
	if m.CurrentBinding != nil {
		v.Binding = m.CurrentBinding
		v.RuntimeName = m.CurrentBinding.RuntimeName
		v.GuestStatus = m.CurrentBinding.GuestManagementStatus
	}
	if m.ActiveOperationID != "" {
		v.Blocked = append(v.Blocked, "operation_in_progress")
	}
	if v.GuestStatus != "ready" && m.ObservedState == "running" {
		v.Blocked = append(v.Blocked, "guest_management_"+v.GuestStatus)
		for _, k := range []string{"persistent_task", "clean_checkpoint", "independent_clone", "active_recovery", "automatic_idle"} {
			v.Capabilities[k] = rt.Capability{Supported: false, ReasonCode: "GUEST_MANAGEMENT_UNAVAILABLE", Message: "Guest Task Manager handshake required"}
		}
	}
	return v, nil
}
