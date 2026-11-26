package runtime

import "context"

type BootMode string

const (
	BootOCI    BootMode = "oci"
	BootSystem BootMode = "system"
)

const (
	SystemBootCapability        = "boot.system"
	SystemBootProfile           = "system-debian-v1"
	LiveComputeGrowthCapability = "boot.live_compute_growth"
	LiveComputeGrowthProfile    = "managed-live-compute-v1"
	Launch                      = "launch"
)

func SystemBootUnavailable(official bool) error {
	return &CapabilityError{Capability: SystemBootCapability, Status: fullStateCapability(official)}
}

// LaunchBinding describes the Worker slot before an execution exists.
type LaunchBinding struct {
	InstanceID     string  `json:"instance_id"`
	IncarnationID  *string `json:"incarnation_id"`
	ExecutionEpoch int64   `json:"execution_epoch"`
}

type LaunchSubmission struct {
	OperationID   string           `json:"operation_id"`
	RequestDigest string           `json:"request_digest"`
	Expected      LaunchBinding    `json:"expected"`
	Kind          string           `json:"kind"`
	Parameters    LaunchParameters `json:"parameters"`
}

type LaunchParameters struct {
	Name              string         `json:"name"`
	BootProfile       string         `json:"boot_profile"`
	LiveComputeGrowth bool           `json:"live_compute_growth,omitempty"`
	CPUs              uint8          `json:"cpus"`
	MemoryMiB         uint32         `json:"memory_mib"`
	Disks             []LaunchDisk   `json:"disks"`
	Init              string         `json:"init"`
	Args              []string       `json:"args"`
	Network           *LaunchNetwork `json:"network,omitempty"`
}

// LaunchNetwork combines the public selection with topology from trusted runtime configuration.
type LaunchNetwork struct {
	NetworkID     string `json:"network_id"`
	Mode          string `json:"mode,omitempty"`
	RouteDomainID string `json:"route_domain_id,omitempty"`
	CIDR          string `json:"cidr,omitempty"`
}

const (
	NativeNetwork        = "native"
	FixedInternalNetwork = "fixed_internal"
)

type LaunchDisk struct {
	AttachmentID string `json:"attachment_id"`
	DeviceID     string `json:"device_id"`
	Role         string `json:"role"`
	GenerationID string `json:"generation_id,omitempty"`
	VolumeID     string `json:"volume_id,omitempty"`
	ReadOnly     bool   `json:"read_only"`
}

type NativeLauncher interface {
	NativeControl
	SystemBoot(context.Context) (Capability, error)
	DiscoverLaunchBinding(context.Context, string) (LaunchBinding, error)
	SubmitLaunch(context.Context, LaunchSubmission) (OperationReceipt, error)
}
