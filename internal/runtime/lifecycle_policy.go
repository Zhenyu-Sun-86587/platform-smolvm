package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type LifecyclePolicy struct {
	SchemaVersion   int            `json:"schema_version"`
	AutoStandby     bool           `json:"auto_standby"`
	StandbyAfter    int            `json:"standby_after_seconds"`
	AutoHibernate   bool           `json:"auto_hibernate"`
	HibernateAfter  int            `json:"hibernate_after_seconds"`
	KeepRunning     bool           `json:"keep_running"`
	ReclaimPriority int            `json:"reclaim_priority"`
	PressureReclaim string         `json:"pressure_reclaim"`
	Wake            WakeConditions `json:"wake_conditions"`
	Activity        ActivityPolicy `json:"activity"`
	MinRunning      int            `json:"min_running_after_wake_seconds"`
	RetryBackoff    int            `json:"retry_backoff_seconds"`
}
type WakeConditions struct {
	API       bool `json:"api_access"`
	Service   bool `json:"service_access"`
	Private   bool `json:"private_network_packet"`
	ColdStart bool `json:"allow_cold_start"`
}
type ActivityPolicy struct {
	Tasks    bool   `json:"protect_running_tasks"`
	Exec     bool   `json:"protect_exec_sessions"`
	Inbound  bool   `json:"protect_inbound_connections"`
	Outbound bool   `json:"protect_outbound_connections"`
	Leases   bool   `json:"protect_running_leases"`
	Unknown  string `json:"unknown"`
}

func DefaultLifecyclePolicy() LifecyclePolicy {
	return LifecyclePolicy{SchemaVersion: 1, AutoStandby: true, StandbyAfter: 300, AutoHibernate: true, HibernateAfter: 1800,
		ReclaimPriority: 50, PressureReclaim: "idle_only", Wake: WakeConditions{API: true, Service: true, Private: true},
		Activity: ActivityPolicy{Tasks: true, Exec: true, Inbound: true, Outbound: true, Leases: true, Unknown: "block_auto_sleep"}, MinRunning: 60, RetryBackoff: 60}
}
func (p LifecyclePolicy) WithPatch(raw json.RawMessage) (LifecyclePolicy, error) {
	if len(raw) > 0 {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '{' {
			return p, fmt.Errorf("%w: lifecycle policy must be an object", ErrInvalid)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&p); err != nil {
			return p, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return p, ErrInvalid
		}
		// An explicit null is neither a policy value nor a request to inherit
		// a later global default. Reject it before persisting the patch.
		var supplied map[string]json.RawMessage
		if err := json.Unmarshal(raw, &supplied); err != nil {
			return p, ErrInvalid
		}
		for key, value := range supplied {
			if bytes.Equal(value, []byte("null")) {
				return p, ErrInvalid
			}
			if key == "wake_conditions" || key == "activity" {
				var nested map[string]json.RawMessage
				if err := json.Unmarshal(value, &nested); err != nil {
					return p, ErrInvalid
				}
				for _, field := range nested {
					if bytes.Equal(field, []byte("null")) {
						return p, ErrInvalid
					}
				}
			}
		}
	}
	const year = 365 * 24 * 60 * 60
	if p.SchemaVersion != 1 || p.ReclaimPriority < 0 || p.ReclaimPriority > 100 || (p.PressureReclaim != "idle_only" && p.PressureReclaim != "disabled") ||
		p.Activity.Unknown != "block_auto_sleep" || !p.Activity.Leases || p.StandbyAfter < 0 || p.StandbyAfter > year || p.HibernateAfter < 0 || p.HibernateAfter > year ||
		p.MinRunning < 0 || p.MinRunning > year || p.RetryBackoff < 1 || p.RetryBackoff > year || p.AutoStandby && p.StandbyAfter == 0 ||
		p.AutoHibernate && p.HibernateAfter == 0 || p.AutoStandby && p.AutoHibernate && p.HibernateAfter < p.StandbyAfter {
		return p, fmt.Errorf("%w: invalid lifecycle policy", ErrInvalid)
	}
	return p, nil
}
