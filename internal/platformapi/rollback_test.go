package platformapi

import (
	"testing"

	rt "sandbox-platform/internal/runtime"
)

func TestInactiveRollbackFence(t *testing.T) {
	cases := []struct {
		name     string
		observed rt.Observation
		expected string
		ok       bool
	}{
		{"created", rt.Observation{State: rt.StateCreated}, rt.StateCreated, true},
		{"stopped", rt.Observation{State: rt.StateStopped}, rt.StateStopped, true},
		{"changed", rt.Observation{State: rt.StateStopped}, rt.StateCreated, false},
		{"running identity", rt.Observation{State: rt.StateStopped, Identity: &rt.Identity{}}, rt.StateStopped, false},
		{"saved", rt.Observation{State: rt.StatePaused}, rt.StatePaused, false},
		{"unknown", rt.Observation{State: rt.StateUnknown}, rt.StateUnknown, false},
		{"pressure race", rt.Observation{State: rt.StateStopped, StateReason: "host memory pressure: 10 MiB available"}, rt.StateStopped, false},
		{"slice pressure race", rt.Observation{State: rt.StateStopped, StateReason: "VM memory slice pressure: 512 MiB of its 512 MiB cap in use"}, rt.StateStopped, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchesInactiveFence(c.observed, c.expected); got != c.ok {
				t.Fatalf("matchesInactiveFence(%+v, %q) = %v, want %v", c.observed, c.expected, got, c.ok)
			}
		})
	}
}
