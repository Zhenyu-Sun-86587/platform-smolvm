package platformapi

import (
	"context"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type platformMetrics struct {
	AccessHolds int            `json:"active_access_holds"`
	WakeWaiters int            `json:"wake_waiters"`
	Operations  map[string]int `json:"operations_by_state"`
}

func (s *Server) platformMetrics(ctx context.Context, machine string) (platformMetrics, error) {
	holds, waiters := s.AccessCounts(machine)
	v := platformMetrics{AccessHolds: holds, WakeWaiters: waiters, Operations: map[string]int{}}
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT state, COUNT(*) FROM operations WHERE (?='' OR machine_id=?) GROUP BY state", machine, machine)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err = rows.Scan(&state, &count); err != nil {
			return v, err
		}
		v.Operations[state] = count
	}
	return v, rows.Err()
}

// scopedMachineMetrics is implemented by runtime adapters that can acquire a
// read-only sample for an exact native execution.
type scopedMachineMetrics interface {
	MachineMetrics(context.Context, string, rt.ExecutionRef) (rt.Metrics, error)
}

func missingMetric(source, scope, code, message string) *rt.MetricObservation {
	return &rt.MetricObservation{
		Availability: "unavailable", Source: source, Scope: scope,
		Issues: map[string]rt.MetricIssue{"runtime": {Code: code, Message: message}},
	}
}

type machineMetrics struct {
	*rt.MetricObservation
	Name           string       `json:"name,omitempty"`
	At             string       `json:"at,omitempty"`
	State          string       `json:"state,omitempty"`
	VCPUs          *uint32      `json:"vcpus,omitempty"`
	MemoryLimitMB  *uint32      `json:"memory_limit_mb,omitempty"`
	CPUMillis      *uint64      `json:"cpu_millis"`
	CPULimitMillis *uint32      `json:"cpu_limit_millicpus"`
	MemoryMB       *uint64      `json:"memory_mb"`
	RSSMB          *uint64      `json:"rss_mb"`
	DiskUsedMB     *uint64      `json:"disk_used_mb"`
	DiskGB         *uint64      `json:"disk_gb"`
	EgressBytes    *uint64      `json:"egress_bytes"`
	Pressure       *rt.Pressure `json:"pressure"`
}

func (s *Server) machineMetricSample(ctx context.Context, m registry.Machine) (machineMetrics, error) {
	if m.CurrentBinding == nil {
		return machineMetrics{MetricObservation: missingMetric("platform", "machine", "BINDING_UNAVAILABLE", "Machine has no current runtime binding.")}, nil
	}
	b := *m.CurrentBinding
	source := "official_runtime"
	native := b.RuntimeBundleID != "" || b.RuntimeInstanceID != ""
	if native {
		source = "worker"
		if b.PowerState != registry.PowerRunning && b.PowerState != registry.PowerStandby {
			return machineMetrics{MetricObservation: missingMetric(source, "machine", "EXECUTION_UNAVAILABLE", "Machine has no current running or standby execution.")}, nil
		}
	}
	adapter, err := s.ForBinding(b)
	if err != nil || adapter == nil {
		return machineMetrics{MetricObservation: missingMetric(source, "machine", "RUNTIME_UNAVAILABLE", "Machine runtime is unavailable.")}, nil
	}
	var sample rt.Metrics
	if native {
		scoped, ok := adapter.(scopedMachineMetrics)
		if !ok {
			return machineMetrics{MetricObservation: missingMetric(source, "machine", "METRICS_UNSUPPORTED", "Machine runtime does not provide execution-scoped metrics.")}, nil
		}
		sample, err = scoped.MachineMetrics(ctx, b.RuntimeName, b.ExecutionRef())
	} else {
		sample, err = adapter.Metrics(ctx, b.RuntimeName)
	}
	if err != nil {
		return machineMetrics{MetricObservation: missingMetric(source, "machine", "METRICS_UNAVAILABLE", "Machine metrics could not be sampled.")}, nil
	}
	current, err := s.DB.GetMachine(ctx, m.ID)
	if err != nil {
		return machineMetrics{}, err
	}
	if c := current.CurrentBinding; current.DeletedAt != nil || c == nil || c.ID != b.ID || c.Generation != b.Generation || c.ExecutionRef() != b.ExecutionRef() || c.PowerState != b.PowerState {
		return machineMetrics{MetricObservation: missingMetric(source, "machine", "EXECUTION_CHANGED", "Machine execution changed during sampling.")}, nil
	}
	o := sample.MetricObservation
	if o == nil {
		o = &rt.MetricObservation{Availability: "available", Source: source, Scope: "machine"}
		if observed, err := time.Parse(time.RFC3339Nano, sample.At); err == nil {
			o.ObservedAt = &observed
		}
		o.Issues = legacyMetricIssues(sample)
		if len(o.Issues) > 0 {
			o.Availability = "partial"
		}
	}
	if native {
		o.Execution = &rt.MetricExecution{BindingID: b.ID, BindingGeneration: b.Generation, IncarnationID: b.IncarnationID, ExecutionEpoch: b.ExecutionEpoch}
	}
	result := machineMetrics{
		MetricObservation: o, Name: sample.Name, At: sample.At, State: sample.State,
		CPUMillis: sample.CPUMillis, CPULimitMillis: sample.CPULimitMillis, MemoryMB: sample.MemoryMB, RSSMB: sample.RSSMB,
		DiskUsedMB: sample.DiskUsedMB, DiskGB: sample.DiskGB, EgressBytes: sample.EgressBytes, Pressure: sample.Pressure,
	}
	if _, missing := o.Issues["vcpus"]; !missing {
		result.VCPUs = &sample.VCPUs
	}
	if _, missing := o.Issues["memory_limit_mb"]; !missing {
		result.MemoryLimitMB = &sample.MemoryLimitMB
	}
	return result, nil
}

func legacyMetricIssues(m rt.Metrics) map[string]rt.MetricIssue {
	issues := map[string]rt.MetricIssue{}
	missing := rt.MetricIssue{Code: "NOT_SAMPLED", Message: "No runtime measurement was returned."}
	for name, present := range map[string]bool{
		"cpu_millis": m.CPUMillis != nil, "cpu_limit_millicpus": m.CPULimitMillis != nil,
		"memory_mb": m.MemoryMB != nil, "rss_mb": m.RSSMB != nil, "disk_used_mb": m.DiskUsedMB != nil,
		"disk_gb": m.DiskGB != nil, "egress_bytes": m.EgressBytes != nil, "pressure": m.Pressure != nil,
	} {
		if !present {
			issues[name] = missing
		}
	}
	return issues
}

type systemMetrics struct {
	*rt.MetricObservation
	AllocatedCPUs         *uint32  `json:"allocated_cpus,omitempty"`
	AllocatedMemoryMB     *uint64  `json:"allocated_memory_mb,omitempty"`
	UsedCPUs              *float64 `json:"used_cpus,omitempty"`
	UsedMemoryMB          *uint64  `json:"used_memory_mb,omitempty"`
	HostMemoryTotalMB     *uint64  `json:"host_memory_total_mb"`
	HostMemoryAvailableMB *uint64  `json:"host_memory_available_mb"`
	UsedDiskGB            *uint64  `json:"used_disk_gb,omitempty"`
	BootID                string   `json:"boot_id,omitempty"`
}

func (s *Server) systemMetricSample(ctx context.Context) (systemMetrics, error) {
	profiles, err := s.DB.ListRuntimeProfiles(ctx)
	if err != nil {
		return systemMetrics{}, err
	}
	source, scope := "official_runtime", "official_runtime"
	adapter := s.OfficialRuntime()
	switch len(profiles) {
	case 0:
	case 1:
		source, scope = "worker", "worker"
		profile := profiles[0]
		adapter, err = s.ForBinding(registry.Binding{RuntimeInstanceID: profile.RuntimeInstanceID, RuntimeBundleID: profile.RuntimeBundleID})
		if err != nil {
			return systemMetrics{MetricObservation: missingMetric(source, scope, "RUNTIME_UNAVAILABLE", "Registered Worker metrics route is unavailable.")}, nil
		}
	default:
		return systemMetrics{MetricObservation: missingMetric("worker", "worker", "WORKER_SCOPE_AMBIGUOUS", "Registered runtime profiles do not identify a single Worker scope.")}, nil
	}
	if adapter == nil {
		return systemMetrics{MetricObservation: missingMetric(source, scope, "RUNTIME_UNAVAILABLE", "System metrics route is unavailable.")}, nil
	}
	sample, err := adapter.Capacity(ctx)
	if err != nil {
		return systemMetrics{MetricObservation: missingMetric(source, scope, "METRICS_UNAVAILABLE", "System metrics could not be sampled.")}, nil
	}
	o := sample.MetricObservation
	if o == nil {
		o = &rt.MetricObservation{Availability: "available", Source: source, Scope: scope}
		if sample.HostMemoryTotalMB == nil || sample.HostMemoryAvailableMB == nil {
			o.Availability = "partial"
		}
	}
	result := systemMetrics{
		MetricObservation: o, HostMemoryTotalMB: sample.HostMemoryTotalMB,
		HostMemoryAvailableMB: sample.HostMemoryAvailableMB, BootID: sample.BootID,
	}
	if _, missing := o.Issues["allocated_cpus"]; !missing {
		result.AllocatedCPUs = &sample.AllocatedCPUs
	}
	if _, missing := o.Issues["allocated_memory_mb"]; !missing {
		result.AllocatedMemoryMB = &sample.AllocatedMemoryMB
	}
	if _, missing := o.Issues["used_cpus"]; !missing {
		result.UsedCPUs = &sample.UsedCPUs
	}
	if _, missing := o.Issues["used_memory_mb"]; !missing {
		result.UsedMemoryMB = &sample.UsedMemoryMB
	}
	if _, missing := o.Issues["used_disk_gb"]; !missing {
		result.UsedDiskGB = &sample.UsedDiskGB
	}
	return result, nil
}

func (s *Server) metricsRoutes() {
	s.route("GET /v1/machines/{machine}/metrics", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(r.Context(), id)
		if err != nil {
			return err
		}
		sample, err := s.machineMetricSample(r.Context(), m)
		if err != nil {
			return err
		}
		platform, err := s.platformMetrics(r.Context(), id)
		if err != nil {
			return err
		}
		if sample.Availability == "unavailable" {
			return write(w, 200, struct {
				*rt.MetricObservation
				Platform platformMetrics `json:"platform"`
			}{sample.MetricObservation, platform})
		}
		return write(w, 200, struct {
			machineMetrics
			Platform platformMetrics `json:"platform"`
		}{sample, platform})
	})
	s.route("GET /v1/system/metrics", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("read", ""); err != nil {
			return err
		}
		sample, err := s.systemMetricSample(r.Context())
		if err != nil {
			return err
		}
		platform, err := s.platformMetrics(r.Context(), "")
		if err != nil {
			return err
		}
		if sample.Availability == "unavailable" {
			return write(w, 200, struct {
				*rt.MetricObservation
				Platform platformMetrics `json:"platform"`
			}{sample.MetricObservation, platform})
		}
		return write(w, 200, struct {
			systemMetrics
			Platform platformMetrics `json:"platform"`
		}{sample, platform})
	})
}
