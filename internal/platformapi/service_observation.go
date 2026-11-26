package platformapi

import (
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/servicehealth"
)

func serviceObservationIdentity(v service, b *registry.Binding) servicehealth.Identity {
	key := servicehealth.Identity{ServiceRevision: v.Revision}
	if b != nil {
		key.BindingID = b.ID
		key.BindingGeneration = b.Generation
		key.IncarnationID = b.IncarnationID
		key.ExecutionEpoch = b.ExecutionEpoch
	}
	return key
}

func servicePower(b *registry.Binding) registry.PowerState {
	if b == nil {
		return registry.PowerUnknown
	}
	if b.RuntimeBundleID == "" && b.RuntimeInstanceID == "" {
		return officialPowerState(*b)
	}
	return b.PowerState
}

func (s *Server) projectServiceObservation(v *service, m registry.Machine) {
	key := serviceObservationIdentity(*v, m.CurrentBinding)
	running := m.DeletedAt == nil && servicePower(m.CurrentBinding) == registry.PowerRunning
	observation := s.serviceHealth.View(v.ID, key, v.Enabled, running)
	v.Observation = &observation
}
