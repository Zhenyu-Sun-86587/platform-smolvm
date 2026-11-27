// Package runtime defines the boundary between platform lifecycle and the VM service.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const Version = "1.37.1"

type State = string

const (
	StateUnknown     State = "unknown"
	StateAbsent      State = "absent"
	StateCreated     State = "created"
	StateRunning     State = "running"
	StateStopped     State = "stopped"
	StatePaused      State = "paused"
	StatePausing     State = "pausing"
	StateFrozen      State = "frozen"
	StateFailed      State = "failed"
	StateUnreachable State = "unreachable"
)

// Identity contains the exact native process identity. It is meaningful only
// when both PID and StartTime are present.
type Identity struct {
	PID       json.RawMessage `json:"pid"`
	StartTime json.RawMessage `json:"start_time"`
	BootID    json.RawMessage `json:"boot_id,omitempty"`
}

func (id *Identity) Complete() bool {
	return id != nil && present(id.PID) && present(id.StartTime)
}

func (id *Identity) Equal(other *Identity) bool {
	return id.Complete() && other.Complete() &&
		bytes.Equal(id.PID, other.PID) && bytes.Equal(id.StartTime, other.StartTime) &&
		bytes.Equal(id.BootID, other.BootID)
}

func present(value json.RawMessage) bool {
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

type Health struct {
	Status                   string   `json:"status"`
	Version                  string   `json:"version"`
	CheckpointDeviceProfiles []string `json:"checkpoint_device_profiles"`
	CheckpointFormatVersions []uint32 `json:"checkpoint_format_versions"`
}

type Port struct {
	Host     uint16 `json:"host"`
	Guest    uint16 `json:"guest"`
	Protocol string `json:"protocol,omitempty"`
}

type Capability struct {
	MemoryFork      *MemoryForkObservation `json:"memory_fork,omitempty"`
	Status          string                 `json:"status,omitempty"`
	ContractVersion int                    `json:"contract_version,omitempty"`
	Constraints     json.RawMessage        `json:"constraints,omitempty"`
	Supported       bool                   `json:"supported"`
	ReasonCode      string                 `json:"reason_code,omitempty"`
	Message         string                 `json:"message,omitempty"`
	VerifiedProfile string                 `json:"verified_profile,omitempty"`
}

type Observation struct {
	Name           string                `json:"name"`
	State          State                 `json:"state"`
	RawState       string                `json:"raw_state"`
	Identity       *Identity             `json:"runtime_identity,omitempty"`
	ParentMachine  string                `json:"parent_machine,omitempty"`
	Branchable     bool                  `json:"branchable"`
	StateReason    string                `json:"state_reason,omitempty"`
	WorkloadError  *string               `json:"workload_error,omitempty"`
	Ports          []Port                `json:"ports"`
	AllowedCIDRs   []string              `json:"allowed_cidrs,omitempty"`
	AllowedHosts   []string              `json:"allowed_hosts,omitempty"`
	EgressRules    []json.RawMessage     `json:"egress_rules,omitempty"`
	ObservedAt     time.Time             `json:"observed_at"`
	Image          string                `json:"image,omitempty"`
	Labels         map[string]string     `json:"labels,omitempty"`
	CPUs           uint8                 `json:"cpus"`
	MemoryMB       uint32                `json:"memory_mb"`
	StorageGB      uint64                `json:"storage_gb"`
	OverlayGB      uint64                `json:"overlay_gb"`
	DiskChainDepth *uint                 `json:"disk_chain_depth,omitempty"`
	Capabilities   map[string]Capability `json:"capabilities,omitempty"`
}

func (o Observation) PressurePaused() bool {
	return o.StateReason == "pressurePaused" ||
		strings.HasPrefix(o.StateReason, "host memory pressure: ") ||
		strings.HasPrefix(o.StateReason, "VM memory slice pressure: ")
}

type CreateRequest struct {
	Name       string
	Image      string
	LocalImage string // Operator-registered basename within the configured image directory.
	CPUs       uint8
	MemoryMB   uint32
	StorageGB  uint64
	OverlayGB  uint64
	Ports      []Port
	Labels     map[string]string
	Command    []string
	EntryPoint []string
	Env        map[string]string
}

type ForkRequest struct {
	Name       string
	Branchable bool
	Ports      []Port
	Labels     map[string]string
}

type Receipt struct {
	Name           string      `json:"name"`
	Observation    Observation `json:"observation"`
	RuntimePauseID string      `json:"runtime_pause_id,omitempty"`
}

var (
	ErrNotFound    = errors.New("runtime object not found")
	ErrUnavailable = errors.New("runtime unavailable")
	ErrConflict    = errors.New("runtime conflict")
	ErrInvalid     = errors.New("invalid runtime input")
	ErrUnsupported = errors.New("unsupported runtime capability")
	ErrUncertain   = errors.New("runtime effect unknown")
)

type Error struct {
	Code       string
	NativeCode string
	Message    string
	Status     int
	Body       string
	Unknown    bool
	Cause      error
}

func (err *Error) Error() string {
	if err.Message == "" {
		return err.Code
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (err *Error) Unwrap() error { return err.Cause }

func (err *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return err.Code == "NOT_FOUND"
	case ErrUnavailable:
		return err.Code == "RUNTIME_UNAVAILABLE"
	case ErrConflict:
		return err.Code == "CONFLICT" || err.Code == "PORT_IN_USE"
	case ErrInvalid:
		return err.Code == "INVALID_ARGUMENT"
	case ErrUnsupported:
		return err.Code == "NOT_SUPPORTED" || err.Code == "RUNTIME_VERSION_MISMATCH"
	case ErrUncertain:
		return err.Unknown
	default:
		return false
	}
}

type ExecRequest struct {
	Command        []string
	Env            map[string]string
	Cwd            string
	User           string
	TimeoutSeconds uint64
	Stdin          string
	Background     bool
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	PID      *int   `json:"pid,omitempty"`
}

type ExecSessionRequest struct {
	ExecRequest
	ID          string
	Interactive bool
	TTY         bool
	Cols        uint16
	Rows        uint16
	WaitSeconds uint64
	MaxBytes    int
}

type OutputChunk struct {
	Stream string `json:"stream"`
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
}

type OutputGap struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

type ExecSession struct {
	ID           string
	State        string
	Done         bool
	ExitCode     *int
	Error        *string
	Output       []OutputChunk
	Next         uint64
	Start        uint64
	End          uint64
	Lost         *OutputGap
	StdinApplied uint64
	TTY          bool
	Interactive  bool
	CreatedAt    uint64
}

type ExecRead struct {
	After uint64
	Wait  uint64
	Max   int
}

type FileOptions struct {
	Size int64 // A negative value requests streaming with unknown length.
	Mode *uint32
	UID  *uint32
	GID  *uint32
}

type ResizeRequest struct {
	OperationID     string
	ExpectedRuntime *Identity
	MemoryMB        uint32
	CPUs            uint8
	StorageGB       uint64
	OverlayGB       uint64
}

type EgressUpdate struct {
	AllowHosts       []string
	AllowCIDRs       []string
	RemoveAllowHosts []string
	RemoveAllowCIDRs []string
}

type Capacity struct {
	*MetricObservation
	AllocatedCPUs         uint32  `json:"allocated_cpus"`
	AllocatedMemoryMB     uint64  `json:"allocated_memory_mb"`
	UsedCPUs              float64 `json:"used_cpus"`
	UsedMemoryMB          uint64  `json:"used_memory_mb"`
	HostMemoryTotalMB     *uint64 `json:"host_memory_total_mb"`
	HostMemoryAvailableMB *uint64 `json:"host_memory_available_mb"`
	UsedDiskGB            uint64  `json:"used_disk_gb"`
	BootID                string  `json:"boot_id"`
}

type PressureSample struct {
	SomeAvg10 float64 `json:"someAvg10"`
	SomeAvg60 float64 `json:"someAvg60"`
	FullAvg10 float64 `json:"fullAvg10"`
	FullAvg60 float64 `json:"fullAvg60"`
}

type Pressure struct {
	CPU    PressureSample `json:"cpu"`
	Memory PressureSample `json:"memory"`
	IO     PressureSample `json:"io"`
}

type Metrics struct {
	*MetricObservation
	Name           string    `json:"name"`
	At             string    `json:"at"`
	State          string    `json:"state"`
	VCPUs          uint32    `json:"vcpus"`
	MemoryLimitMB  uint32    `json:"memory_limit_mb"`
	CPUMillis      *uint64   `json:"cpu_millis"`
	CPULimitMillis *uint32   `json:"cpu_limit_millicpus"`
	MemoryMB       *uint64   `json:"memory_mb"`
	RSSMB          *uint64   `json:"rss_mb"`
	DiskUsedMB     *uint64   `json:"disk_used_mb"`
	DiskGB         *uint64   `json:"disk_gb"`
	EgressBytes    *uint64   `json:"egress_bytes"`
	Pressure       *Pressure `json:"pressure"`
}
