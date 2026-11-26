package smolvm

import (
	"context"
	"net/http"
	"net/netip"

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
	var result runtime.Capacity
	err := client.json(ctx, http.MethodGet, "/capacity", nil, nil, &result)
	return result, err
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
		VCPUs          uint32            `json:"vcpus"`
		MemoryLimitMB  uint32            `json:"memoryLimitMb"`
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
	return runtime.Metrics{
		Name: native.Name, At: native.At, State: native.State, VCPUs: native.VCPUs, MemoryLimitMB: native.MemoryLimitMB,
		CPUMillis: native.CPUMillis, CPULimitMillis: native.CPULimitMillis, MemoryMB: native.MemoryMB, RSSMB: native.RSSMB,
		DiskUsedMB: native.DiskUsedMB, DiskGB: native.DiskGB, EgressBytes: native.EgressBytes, Pressure: native.Pressure,
	}, nil
}
