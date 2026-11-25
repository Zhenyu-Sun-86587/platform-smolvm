package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sandbox-platform/internal/runtime"
)

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
	pauseIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	imagePattern      = regexp.MustCompile(`^[a-z0-9]+(?:[._-]+[a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-]+[a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	localImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func ValidateName(name string) error {
	if !namePattern.MatchString(name) || strings.Contains(name, "--") || strings.HasSuffix(name, "-") {
		return invalid("runtime name must start with a lowercase letter or digit and contain at most 128 lowercase letters, digits, underscores or nonconsecutive hyphens")
	}
	return nil
}

func machinePath(name, suffix string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return machinesPath + "/" + name + suffix, nil
}

type nativeIdentity struct {
	PID       json.RawMessage `json:"pid"`
	StartTime json.RawMessage `json:"startTime"`
	BootID    json.RawMessage `json:"bootId,omitempty"`
}

type machineInfo struct {
	Name           string            `json:"name"`
	State          string            `json:"state"`
	Identity       *nativeIdentity   `json:"runtime"`
	PID            json.RawMessage   `json:"pid"`
	ParentMachine  string            `json:"parentMachine"`
	Branchable     bool              `json:"branchable"`
	StateReason    string            `json:"stateReason"`
	WorkloadError  *string           `json:"workloadError"`
	Ports          []runtime.Port    `json:"ports"`
	AllowedCIDRs   []string          `json:"allowedCidrs"`
	AllowedHosts   []string          `json:"allowedHosts"`
	EgressRules    []json.RawMessage `json:"egressRules"`
	Image          string            `json:"image"`
	Labels         map[string]string `json:"labels"`
	CPUs           uint8             `json:"cpus"`
	MemoryMB       uint32            `json:"memoryMb"`
	StorageGB      uint64            `json:"storageGb"`
	OverlayGB      uint64            `json:"overlayGb"`
	DiskChainDepth *uint             `json:"diskChainDepth"`
	NetworkBackend string            `json:"networkBackend"`
	DiskDurability string            `json:"diskDurability"`
	Mounts         []json.RawMessage `json:"mounts"`
	CacheDisk      *json.RawMessage  `json:"cacheDisk"`
	GPU            bool              `json:"gpu"`
	CUDA           bool              `json:"cuda"`
}

func (m machineInfo) observe() runtime.Observation {
	var identity *runtime.Identity
	if m.Identity != nil {
		identity = &runtime.Identity{PID: m.Identity.PID, StartTime: m.Identity.StartTime, BootID: m.Identity.BootID}
	}
	state := m.State
	switch state {
	case runtime.StateRunning, runtime.StateFrozen, runtime.StateUnreachable:
		if !identity.Complete() || (len(m.PID) != 0 && !bytes.Equal(m.PID, []byte("null")) && !bytes.Equal(m.PID, identity.PID)) {
			state = runtime.StateUnknown
			identity = nil
		}
	case runtime.StateCreated, runtime.StateStopped, runtime.StatePaused, runtime.StatePausing, runtime.StateFailed:
	default:
		state = runtime.StateUnknown
	}
	for i := range m.Ports {
		if m.Ports[i].Protocol == "" {
			m.Ports[i].Protocol = "tcp"
		}
	}
	unsupportedProfile := m.NetworkBackend != "virtio-net" || m.DiskDurability != "full" || len(m.Mounts) != 0 || m.CacheDisk != nil || m.GPU || m.CUDA
	capabilities := make(map[string]runtime.Capability)
	for _, capability := range []string{"live_fork", "pause", "checkpoint", "restore", "live_ports", "resize"} {
		capabilities[capability] = runtime.Capability{ReasonCode: "CAPABILITY_NOT_VERIFIED", Message: "Runtime and template acceptance evidence is required."}
		if unsupportedProfile {
			capabilities[capability] = runtime.Capability{ReasonCode: "UNSUPPORTED_PROFILE", Message: "Requires internal storage, virtio-net, full durability, and no host mounts or external devices."}
		}
	}
	return runtime.Observation{
		Name: m.Name, State: state, RawState: m.State, Identity: identity, ParentMachine: m.ParentMachine,
		Branchable: m.Branchable, StateReason: m.StateReason, WorkloadError: m.WorkloadError, Ports: m.Ports, ObservedAt: time.Now().UTC(),
		AllowedCIDRs: m.AllowedCIDRs, AllowedHosts: m.AllowedHosts, EgressRules: m.EgressRules,
		Image: m.Image, Labels: m.Labels, CPUs: m.CPUs, MemoryMB: m.MemoryMB, StorageGB: m.StorageGB,
		OverlayGB: m.OverlayGB, DiskChainDepth: m.DiskChainDepth, Capabilities: capabilities,
	}
}

func (client *Client) Observe(ctx context.Context, name string) (runtime.Observation, error) {
	path, err := machinePath(name, "")
	if err != nil {
		return runtime.Observation{}, err
	}
	observation, err := client.machine(ctx, http.MethodGet, path, name, nil, nil)
	if errors.Is(err, runtime.ErrNotFound) {
		return runtime.Observation{Name: name, State: runtime.StateAbsent, ObservedAt: time.Now().UTC()}, err
	}
	return observation, err
}

func (client *Client) List(ctx context.Context) ([]runtime.Observation, error) {
	var result struct {
		Machines []machineInfo `json:"machines"`
	}
	if err := client.json(ctx, http.MethodGet, machinesPath, nil, nil, &result); err != nil {
		return nil, err
	}
	if result.Machines == nil {
		return nil, protocolError("runtime machine list is missing", false, nil)
	}
	observations := make([]runtime.Observation, 0, len(result.Machines))
	for _, machine := range result.Machines {
		if machine.Name == "" || machine.State == "" {
			return nil, protocolError("runtime machine list is missing a machine name or state", false, nil)
		}
		observations = append(observations, machine.observe())
	}
	return observations, nil
}

func (client *Client) machine(ctx context.Context, method, path, expectedName string, query url.Values, input any) (runtime.Observation, error) {
	var result machineInfo
	if err := client.json(ctx, method, path, query, input, &result); err != nil {
		return runtime.Observation{}, err
	}
	return result.expected(expectedName, method != http.MethodGet)
}

func (m machineInfo) expected(name string, mutates bool) (runtime.Observation, error) {
	if m.Name != name || m.State == "" {
		return runtime.Observation{}, protocolError("runtime response did not identify the expected machine and state", mutates, nil)
	}
	return m.observe(), nil
}

// DecodeObservation reads the public machine response over a verified connection.
func DecodeObservation(raw json.RawMessage, name string) (runtime.Observation, error) {
	var result machineInfo
	if err := json.Unmarshal(raw, &result); err != nil {
		return runtime.Observation{}, protocolError("decoding runtime machine observation", false, err)
	}
	return result.expected(name, false)
}

type createBody struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	CPUs           uint8             `json:"cpus,omitempty"`
	MemoryMB       uint32            `json:"memoryMb,omitempty"`
	StorageGB      uint64            `json:"storageGb,omitempty"`
	OverlayGB      uint64            `json:"overlayGb,omitempty"`
	Ports          []runtime.Port    `json:"ports"`
	Labels         map[string]string `json:"labels,omitempty"`
	Command        []string          `json:"cmd,omitempty"`
	EntryPoint     []string          `json:"entrypoint,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	NetworkBackend string            `json:"networkBackend"`
	DiskDurability string            `json:"diskDurability"`
	AllowedCIDRs   []string          `json:"allowedCidrs"`
	EgressRules    []struct{}        `json:"egressRules"`
	Restart        struct {
		Policy string `json:"policy"`
	} `json:"restart"`
}

func (client *Client) Create(ctx context.Context, request runtime.CreateRequest) (runtime.Observation, error) {
	if err := ValidateName(request.Name); err != nil {
		return runtime.Observation{}, err
	}
	image := request.Image
	if request.LocalImage != "" {
		if image != "" || client.localImageDir == "" || !localImagePattern.MatchString(request.LocalImage) {
			return runtime.Observation{}, invalid("local template must be an operator-registered basename in the runtime image directory")
		}
		image = filepath.Join(client.localImageDir, request.LocalImage)
	} else if !imagePattern.MatchString(image) {
		return runtime.Observation{}, invalid("template image must be an OCI reference pinned by SHA-256 digest")
	}
	ports, err := checkedPorts(request.Ports)
	if err != nil {
		return runtime.Observation{}, err
	}
	body := createBody{
		Name: request.Name, Image: image, CPUs: request.CPUs, MemoryMB: request.MemoryMB,
		StorageGB: request.StorageGB, OverlayGB: request.OverlayGB, Ports: ports, Labels: request.Labels,
		Command: request.Command, EntryPoint: request.EntryPoint, Env: request.Env,
		NetworkBackend: "virtio-net", DiskDurability: "full", AllowedCIDRs: publicCIDRs(), EgressRules: []struct{}{},
	}
	body.Restart.Policy = "never"
	return client.machine(ctx, http.MethodPost, machinesPath, request.Name, nil, body)
}

func publicCIDRs() []string { return []string{"0.0.0.0/0", "::/0"} }

func checkedPorts(ports []runtime.Port) ([]runtime.Port, error) {
	result := make([]runtime.Port, len(ports))
	hosts := make(map[uint16]bool, len(ports))
	for i, port := range ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}
		if port.Protocol != "tcp" || port.Host == 0 || port.Guest == 0 || hosts[port.Host] {
			return nil, invalid("forwarding requires nonzero TCP ports and unique host ports")
		}
		result[i] = port
		hosts[port.Host] = true
	}
	return result, nil
}

func (client *Client) lifecycle(ctx context.Context, name, action, pauseID string, input any) (runtime.Receipt, error) {
	path, err := machinePath(name, "/"+action)
	if err != nil {
		return runtime.Receipt{}, err
	}
	query := make(url.Values)
	if pauseID != "" {
		if !pauseIDPattern.MatchString(pauseID) {
			return runtime.Receipt{}, invalid("invalid native pause ID")
		}
		query.Set("operation_id", pauseID)
	}
	receipt := runtime.Receipt{Name: name, RuntimePauseID: pauseID}
	receipt.Observation, err = client.machine(ctx, http.MethodPost, path, name, query, input)
	return receipt, err
}

func (client *Client) Start(ctx context.Context, name string) (runtime.Receipt, error) {
	return client.lifecycle(ctx, name, "start", "", struct {
		Branchable bool `json:"branchable"`
	}{true})
}

func (client *Client) Stop(ctx context.Context, name string) (runtime.Receipt, error) {
	stop := func(ctx context.Context) (runtime.Receipt, error) {
		return client.lifecycle(ctx, name, "stop", "", nil)
	}
	receipt, err := stop(ctx)
	return retryAttachment(ctx, client, name, executionHandleChanged, receipt, err, stop)
}

func (client *Client) Pause(ctx context.Context, name, pauseID string) (runtime.Receipt, error) {
	if pauseID == "" {
		return runtime.Receipt{}, invalid("pause requires a native pause ID")
	}
	return client.lifecycle(ctx, name, "pause", pauseID, nil)
}

// Resume with an empty ID resumes a current pressure pause. The caller must
// first observe the paused machine under its durable lifecycle lock.
func (client *Client) Resume(ctx context.Context, name, pauseID string) (runtime.Receipt, error) {
	return client.lifecycle(ctx, name, "resume", pauseID, nil)
}

func (client *Client) Delete(ctx context.Context, name string) (runtime.Receipt, error) {
	remove := func(ctx context.Context) (runtime.Receipt, error) { return client.delete(ctx, name) }
	receipt, err := remove(ctx)
	return retryAttachment(ctx, client, name, executionHandleChanged, receipt, err, remove)
}

func (client *Client) delete(ctx context.Context, name string) (runtime.Receipt, error) {
	path, err := machinePath(name, "")
	if err != nil {
		return runtime.Receipt{}, err
	}
	var result struct {
		Deleted string `json:"deleted"`
	}
	err = client.json(ctx, http.MethodDelete, path, url.Values{"cascade": {"false"}}, nil, &result)
	if err == nil && result.Deleted != name {
		err = protocolError("runtime did not acknowledge deletion of the expected name", true, nil)
	}
	return runtime.Receipt{Name: name}, err
}

func (client *Client) Fork(ctx context.Context, parent string, request runtime.ForkRequest) (runtime.Observation, error) {
	path, err := machinePath(parent, "/branches")
	if err != nil {
		return runtime.Observation{}, err
	}
	if err := ValidateName(request.Name); err != nil {
		return runtime.Observation{}, err
	}
	if request.Name == parent {
		return runtime.Observation{}, invalid("branch must have a new runtime name")
	}
	ports, err := checkedPorts(request.Ports)
	if err != nil {
		return runtime.Observation{}, err
	}
	body := struct {
		Name       string            `json:"name"`
		Branchable bool              `json:"branchable"`
		Ports      []runtime.Port    `json:"ports"`
		Labels     map[string]string `json:"labels,omitempty"`
	}{request.Name, request.Branchable, ports, request.Labels}
	observation, err := client.machine(ctx, http.MethodPost, path, request.Name, nil, body)
	if err == nil && observation.ParentMachine != parent {
		return observation, protocolError(fmt.Sprintf("branch parent %q does not match expected runtime %q", observation.ParentMachine, parent), true, nil)
	}
	return observation, err
}
