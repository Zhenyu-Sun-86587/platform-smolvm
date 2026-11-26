package smolvm

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"sandbox-platform/internal/runtime"
)

func (client *Client) SetPorts(ctx context.Context, name string, ports []runtime.Port) (runtime.Observation, error) {
	path, err := machinePath(name, "/ports")
	if err != nil {
		return runtime.Observation{}, err
	}
	ports, err = checkedPorts(ports)
	if err != nil {
		return runtime.Observation{}, err
	}
	body := struct {
		Ports []runtime.Port `json:"ports"`
	}{ports}
	return client.machine(ctx, http.MethodPut, path, name, nil, body)
}

func (client *Client) SetLabels(ctx context.Context, name string, labels map[string]*string) (runtime.Observation, error) {
	path, err := machinePath(name, "/labels")
	if err != nil {
		return runtime.Observation{}, err
	}
	if len(labels) == 0 {
		return runtime.Observation{}, invalid("at least one label change is required")
	}
	body := struct {
		Labels map[string]*string `json:"labels"`
	}{labels}
	return client.machine(ctx, http.MethodPatch, path, name, nil, body)
}

func (client *Client) EnablePublicNetwork(ctx context.Context, name string) (runtime.Observation, error) {
	return client.SetNetwork(ctx, name, publicCIDRs(), nil)
}

func (client *Client) SetNetwork(ctx context.Context, name string, cidrs, hosts []string) (runtime.Observation, error) {
	path, err := machinePath(name, "/network")
	if err != nil {
		return runtime.Observation{}, err
	}
	if err := validateCIDRs(cidrs); err != nil {
		return runtime.Observation{}, err
	}
	body := struct {
		Mode  string   `json:"mode"`
		CIDRs []string `json:"cidrs"`
		Hosts []string `json:"hosts"`
	}{"allowCidrs", append([]string{}, cidrs...), append([]string{}, hosts...)}
	return client.machine(ctx, http.MethodPut, path, name, nil, body)
}

func validateCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return invalid("egress rules must use IPv4 or IPv6 CIDRs")
		}
	}
	return nil
}

func (client *Client) UpdateEgress(ctx context.Context, name string, update runtime.EgressUpdate) (runtime.Observation, error) {
	path, err := machinePath(name, "/egress")
	if err != nil {
		return runtime.Observation{}, err
	}
	if err := validateCIDRs(update.AllowCIDRs); err != nil {
		return runtime.Observation{}, err
	}
	if err := validateCIDRs(update.RemoveAllowCIDRs); err != nil {
		return runtime.Observation{}, err
	}
	body := struct {
		AllowHosts       []string `json:"allowHosts,omitempty"`
		AllowCIDRs       []string `json:"allowCidrs,omitempty"`
		RemoveAllowHosts []string `json:"removeAllowHosts,omitempty"`
		RemoveAllowCIDRs []string `json:"removeAllowCidrs,omitempty"`
	}{update.AllowHosts, update.AllowCIDRs, update.RemoveAllowHosts, update.RemoveAllowCIDRs}
	return client.machine(ctx, http.MethodPost, path, name, nil, body)
}

func (client *Client) SetCPULimit(ctx context.Context, name string, millicpus *uint32) (runtime.Observation, error) {
	path, err := machinePath(name, "/cpu-limit")
	if err != nil {
		return runtime.Observation{}, err
	}
	if millicpus != nil && *millicpus == 0 {
		return runtime.Observation{}, invalid("CPU limit must be positive or unset")
	}
	body := struct {
		Millicpus *uint32 `json:"millicpus"`
	}{millicpus}
	return client.machine(ctx, http.MethodPut, path, name, nil, body)
}

func (client *Client) Resize(ctx context.Context, name string, request runtime.ResizeRequest) (runtime.Observation, error) {
	path, err := machinePath(name, "/resize")
	if err != nil {
		return runtime.Observation{}, err
	}
	var identity *nativeIdentity
	if request.ExpectedRuntime != nil || request.OperationID != "" {
		if !request.ExpectedRuntime.Complete() || !pauseIDPattern.MatchString(request.OperationID) {
			return runtime.Observation{}, invalid("resize requires a complete runtime identity and a valid operation ID")
		}
		expected := request.ExpectedRuntime
		identity = &nativeIdentity{PID: expected.PID, StartTime: expected.StartTime, BootID: expected.BootID}
	} else if request.CPUs != 0 || request.MemoryMB != 0 {
		return runtime.Observation{}, invalid("CPU and memory resize require a running runtime identity")
	}
	if request.CPUs == 0 && request.MemoryMB == 0 && request.StorageGB == 0 && request.OverlayGB == 0 {
		return runtime.Observation{}, invalid("at least one resource size is required")
	}
	body := struct {
		OperationID     string          `json:"operationId,omitempty"`
		ExpectedRuntime *nativeIdentity `json:"expectedRuntime,omitempty"`
		MemoryMB        uint32          `json:"memoryMb,omitempty"`
		CPUs            uint8           `json:"cpus,omitempty"`
		StorageGB       uint64          `json:"diskGb,omitempty"`
		OverlayGB       uint64          `json:"overlayGb,omitempty"`
	}{request.OperationID, identity, request.MemoryMB, request.CPUs, request.StorageGB, request.OverlayGB}
	return client.machine(ctx, http.MethodPost, path, name, nil, body)
}

func (client *Client) Capacity(ctx context.Context) (runtime.Capacity, error) {
	var native struct {
		AllocatedCPUs         *uint32  `json:"allocated_cpus"`
		AllocatedMemoryMB     *uint64  `json:"allocated_memory_mb"`
		UsedCPUs              *float64 `json:"used_cpus"`
		UsedMemoryMB          *uint64  `json:"used_memory_mb"`
		HostMemoryTotalMB     *uint64  `json:"host_memory_total_mb"`
		HostMemoryAvailableMB *uint64  `json:"host_memory_available_mb"`
		UsedDiskGB            *uint64  `json:"used_disk_gb"`
		BootID                *string  `json:"boot_id"`
	}
	if err := client.json(ctx, http.MethodGet, "/capacity", nil, nil, &native); err != nil {
		return runtime.Capacity{}, err
	}
	o := officialMetricObservation("official_runtime")
	result := runtime.Capacity{MetricObservation: o, HostMemoryTotalMB: native.HostMemoryTotalMB, HostMemoryAvailableMB: native.HostMemoryAvailableMB}
	readOfficialMetric(o, "allocated_cpus", native.AllocatedCPUs, &result.AllocatedCPUs)
	readOfficialMetric(o, "allocated_memory_mb", native.AllocatedMemoryMB, &result.AllocatedMemoryMB)
	readOfficialMetric(o, "used_cpus", native.UsedCPUs, &result.UsedCPUs)
	readOfficialMetric(o, "used_memory_mb", native.UsedMemoryMB, &result.UsedMemoryMB)
	readOfficialMetric(o, "host_memory_total_mb", native.HostMemoryTotalMB, nil)
	readOfficialMetric(o, "host_memory_available_mb", native.HostMemoryAvailableMB, nil)
	readOfficialMetric(o, "used_disk_gb", native.UsedDiskGB, &result.UsedDiskGB)
	readOfficialMetric(o, "boot_id", native.BootID, &result.BootID)
	if len(o.Issues) == 8 {
		o.Availability = "unavailable"
	}
	return result, nil
}

func (client *Client) Metrics(ctx context.Context, name string) (runtime.Metrics, error) {
	path, err := machinePath(name, "/metrics")
	if err != nil {
		return runtime.Metrics{}, err
	}
	var native struct {
		Name           string            `json:"machineId"`
		At             string            `json:"at"`
		State          string            `json:"state"`
		VCPUs          *uint32           `json:"vcpus"`
		MemoryLimitMB  *uint32           `json:"memoryLimitMb"`
		CPUMillis      *uint64           `json:"cpuMillis"`
		CPULimitMillis *uint32           `json:"cpuLimitMillicpus"`
		MemoryMB       *uint64           `json:"memoryMb"`
		RSSMB          *uint64           `json:"rssMb"`
		DiskUsedMB     *uint64           `json:"diskUsedMb"`
		DiskGB         *uint64           `json:"diskGb"`
		EgressBytes    *uint64           `json:"egressBytes"`
		Pressure       *runtime.Pressure `json:"pressure"`
	}
	if err := client.json(ctx, http.MethodGet, path, nil, nil, &native); err != nil {
		return runtime.Metrics{}, err
	}
	if native.Name != name || native.At == "" {
		return runtime.Metrics{}, protocolError("metrics response is missing the expected runtime and timestamp", false, nil)
	}
	o := officialMetricObservation("machine")
	if observed, err := time.Parse(time.RFC3339Nano, native.At); err == nil {
		o.ObservedAt = &observed
	}
	result := runtime.Metrics{
		MetricObservation: o, Name: native.Name, At: native.At, State: native.State,
		CPUMillis: native.CPUMillis, CPULimitMillis: native.CPULimitMillis, MemoryMB: native.MemoryMB, RSSMB: native.RSSMB,
		DiskUsedMB: native.DiskUsedMB, DiskGB: native.DiskGB, EgressBytes: native.EgressBytes, Pressure: native.Pressure,
	}
	readOfficialMetric(o, "vcpus", native.VCPUs, &result.VCPUs)
	readOfficialMetric(o, "memory_limit_mb", native.MemoryLimitMB, &result.MemoryLimitMB)
	readOfficialMetric(o, "cpu_millis", native.CPUMillis, nil)
	readOfficialMetric(o, "cpu_limit_millicpus", native.CPULimitMillis, nil)
	readOfficialMetric(o, "memory_mb", native.MemoryMB, nil)
	readOfficialMetric(o, "rss_mb", native.RSSMB, nil)
	readOfficialMetric(o, "disk_used_mb", native.DiskUsedMB, nil)
	readOfficialMetric(o, "disk_gb", native.DiskGB, nil)
	readOfficialMetric(o, "egress_bytes", native.EgressBytes, nil)
	readOfficialMetric(o, "pressure", native.Pressure, nil)
	return result, nil
}

func officialMetricObservation(scope string) *runtime.MetricObservation {
	return &runtime.MetricObservation{Availability: "available", Source: "official_runtime", Scope: scope, Issues: map[string]runtime.MetricIssue{}}
}

func readOfficialMetric[T any](observation *runtime.MetricObservation, field string, received, dest *T) {
	if received == nil {
		observation.Availability = "partial"
		observation.Issues[field] = runtime.MetricIssue{Code: "NOT_SAMPLED", Message: "No runtime measurement was returned."}
	} else if dest != nil {
		*dest = *received
	}
}
