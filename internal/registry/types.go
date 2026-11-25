package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	rt "sandbox-platform/internal/runtime"
)

var (
	ErrNotFound         = errors.New("not found")
	ErrConflict         = errors.New("object already exists")
	ErrLocked           = errors.New("registry writer already running")
	ErrControlEpoch     = errors.New("control epoch changed")
	ErrMaintenance      = errors.New("control plane in maintenance")
	ErrIdempotency      = errors.New("idempotency key reused for a different request")
	ErrRevision         = errors.New("object revision changed")
	ErrGeneration       = errors.New("binding generation changed")
	ErrInvalid          = errors.New("invalid request")
	ErrOperationActive  = errors.New("machine has an active operation")
	ErrOperationState   = errors.New("operation state changed")
	ErrHeavyBusy        = errors.New("heavy operation limit reached")
	ErrCursorExpired    = errors.New("event cursor expired")
	ErrStaleObservation = errors.New("stale binding observation")
	ErrUnauthorized     = errors.New("principal or credential unavailable")
)

type ActiveOperationError struct {
	MachineID   string
	OperationID string
}

func (e *ActiveOperationError) Error() string {
	return fmt.Sprintf("%s has active operation %s", e.MachineID, e.OperationID)
}
func (e *ActiveOperationError) Unwrap() error { return ErrOperationActive }

type Control struct {
	SchemaVersion  int    `json:"schema_version"`
	InstallationID string `json:"installation_id"`
	ControlEpoch   string `json:"control_epoch"`
	Maintenance    bool   `json:"maintenance"`
	RecoveryReason string `json:"recovery_reason,omitempty"`
}

type Template struct {
	ID           string          `json:"template_id"`
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	ImageDigest  string          `json:"image_digest"`
	GuestRelease string          `json:"guest_release,omitempty"`
	Spec         json.RawMessage `json:"spec"`
	Capabilities json.RawMessage `json:"capabilities"`
	VerifiedAt   *time.Time      `json:"verified_at,omitempty"`
}

type Machine struct {
	ID                 string          `json:"machine_id"`
	Name               string          `json:"name"`
	TemplateID         string          `json:"template_id"`
	Revision           int64           `json:"revision"`
	BindingGeneration  int64           `json:"binding_generation"`
	DesiredPower       string          `json:"desired_power"`
	Spec               json.RawMessage `json:"spec"`
	IdlePolicy         json.RawMessage `json:"idle_policy"`
	KeepRunning        bool            `json:"keep_running"`
	LastUserActivityAt *time.Time      `json:"last_user_activity_at,omitempty"`
	DeletedAt          *time.Time      `json:"deleted_at,omitempty"`
	ActiveOperationID  string          `json:"operation_id,omitempty"`
	CurrentBinding     *Binding        `json:"-"`
	ObservedState      string          `json:"observed_state"`
	Availability       string          `json:"availability"`
	ObservedAt         *time.Time      `json:"observed_at,omitempty"`
}

type PowerState string

const (
	PowerRunning    PowerState = "running"
	PowerStandby    PowerState = "standby"
	PowerHibernated PowerState = "hibernated"
	PowerStopped    PowerState = "stopped"
	PowerUnknown    PowerState = "unknown"
)

type Binding struct {
	RuntimeInstanceID       string          `json:"runtime_instance_id,omitempty"`
	RuntimeBundleID         string          `json:"runtime_bundle_id,omitempty"`
	RuntimeInstanceObjectID string          `json:"runtime_instance_object_id,omitempty"`
	IncarnationID           string          `json:"incarnation_id,omitempty"`
	ExecutionEpoch          int64           `json:"execution_epoch,omitempty"`
	PowerState              PowerState      `json:"power_state"`
	Health                  json.RawMessage `json:"health"`
	CapabilityProfileID     string          `json:"capability_profile_id,omitempty"`
	LastCaptureID           string          `json:"last_capture_id,omitempty"`
	ID                      string          `json:"binding_id"`
	MachineID               string          `json:"machine_id"`
	Generation              int64           `json:"binding_generation"`
	RuntimeName             string          `json:"runtime_name"`
	Status                  string          `json:"status"`
	TaskNamespace           string          `json:"task_namespace"`
	SpecHash                string          `json:"spec_hash"`
	Observation
}

func (b Binding) ExecutionRef() rt.ExecutionRef {
	return rt.ExecutionRef{InstanceID: b.RuntimeInstanceObjectID, IncarnationID: b.IncarnationID, ExecutionEpoch: b.ExecutionEpoch}
}

type NativeObservation struct {
	Execution              rt.ExecutionRef
	PowerState             PowerState
	VerifiedStandbyProfile string
	VerifiedLaunch         bool
	ForkCapabilities       map[string]rt.Capability
	VerifiedRestore        bool
	HibernatedCapture      string
}

// Observation is current public-runtime evidence. Empty optional fields in an
// update preserve existing pause and guest evidence except where explicitly
// replaced. A sampled identity belongs to the observed execution only.
type Observation struct {
	ObservedState           string          `json:"observed_state"`
	NativeState             string          `json:"native_state,omitempty"`
	Availability            string          `json:"availability"`
	RuntimeIdentity         json.RawMessage `json:"runtime_identity,omitempty"`
	ObservedAt              *time.Time      `json:"observed_at,omitempty"`
	RuntimePauseID          string          `json:"runtime_pause_id,omitempty"`
	PauseStatus             string          `json:"pause_status,omitempty"`
	Ports                   json.RawMessage `json:"ports,omitempty"`
	GuestManagementStatus   string          `json:"guest_management_status"`
	GuestManagementEvidence json.RawMessage `json:"guest_management_evidence"`
	StateReason             json.RawMessage `json:"state_reason,omitempty"`
	// A confirmed Start can set the workload failure or clear it with an empty message.
	WorkloadError *string `json:"-"`
	// ExpectedRuntimeIdentity, when set, prevents a probe of a replaced
	// execution from overwriting the evidence for the current execution.
	ExpectedRuntimeIdentity json.RawMessage `json:"-"`
	// ExpectedExecutionEpoch also guards the transition from no native execution.
	ExpectedExecutionEpoch *int64             `json:"-"`
	Native                 *NativeObservation `json:"-"`
}

const (
	Queued           = "queued"
	Dispatching      = "dispatching"
	Observing        = "observing"
	Succeeded        = "succeeded"
	Failed           = "failed"
	Cancelled        = "cancelled"
	RecoveryRequired = "recovery_required"

	EffectNone    = "none"
	EffectApplied = "applied"
	EffectPartial = "partial"
	EffectUnknown = "unknown"

	WorkloadStartFailed = "WORKLOAD_START_FAILED"
)

type Operation struct {
	ID                 string            `json:"operation_id"`
	ControlEpoch       string            `json:"control_epoch"`
	PrincipalID        string            `json:"-"`
	MachineID          string            `json:"machine_id,omitempty"`
	SourceMachineID    string            `json:"source_machine_id,omitempty"`
	BindingID          string            `json:"binding_id,omitempty"`
	BindingGeneration  int64             `json:"binding_generation"`
	RuntimeName        string            `json:"runtime_name,omitempty"`
	Kind               string            `json:"kind"`
	State              string            `json:"state"`
	Stage              string            `json:"stage"`
	Request            json.RawMessage   `json:"-"`
	Dispatch           json.RawMessage   `json:"-"`
	Result             json.RawMessage   `json:"result"`
	Error              json.RawMessage   `json:"error"`
	Effect             string            `json:"effect"`
	SourceDisposition  SourceDisposition `json:"source_disposition,omitempty"`
	SourceLockReleased bool              `json:"source_lock_released,omitempty"`
	Children           []OperationChild  `json:"children,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Version            int64             `json:"-"`
	Heavy              bool              `json:"-"`
	WakePending        bool              `json:"-"`
}

type SourceDisposition string

const (
	SourceRunning SourceDisposition = "RUNNING"
	SourceStandby SourceDisposition = "STANDBY"
	SourceHeld    SourceDisposition = "HELD"
	SourceExited  SourceDisposition = "EXITED"
	SourceUnknown SourceDisposition = "UNKNOWN"
)

type OperationChild struct {
	Ordinal              int                   `json:"ordinal"`
	MachineID            string                `json:"machine_id"`
	BindingID            string                `json:"binding_id"`
	State                string                `json:"state"`
	ResourceIndependence string                `json:"resource_independence,omitempty"`
	ErrorCode            string                `json:"error_code,omitempty"`
	ResourceProof        []rt.ForkDiskEvidence `json:"-"`
}

func (o Operation) Terminal() bool {
	return o.State == Succeeded || o.State == Failed || o.State == Cancelled
}

type Lock struct {
	MachineID          string
	ExpectedGeneration int64
	Role               string
}

type AcceptRequest struct {
	PrincipalID        string
	CredentialID       string
	PrincipalScope     json.RawMessage
	Epoch              string
	Key                string
	Scope              string
	Kind               string
	MachineID          string
	Name               string
	TemplateID         string
	ExpectedRevision   int64
	ExpectedGeneration int64
	Request            json.RawMessage
	MachineSpec        json.RawMessage
	IdlePolicy         json.RawMessage
	DefaultIdlePolicy  json.RawMessage // resolved for a new machine, outside the caller fingerprint
	DesiredPower       string
	KeepRunning        bool
	Heavy              bool
	AllocateBinding    bool
	RuntimeInstanceID  string
	ForkChildren       []string
	Locks              []Lock
	// AdmitRuntime checks local dispatch support without doing I/O in the transaction.
	AdmitRuntime func(rt.BundleRoute, string) error
	// AdmitNative checks the binding's persisted capability and execution evidence.
	AdmitNative func(Binding, string) error
	// ResolveMachine resolves a machine-backed object in the acceptance transaction.
	// On replay it receives the original machine ID for authorization even if the object is gone.
	ResolveMachine func(context.Context, *sql.Tx, string) (string, error)
}

type TransitionRequest struct {
	State             string
	Stage             string
	Effect            string
	SourceDisposition SourceDisposition
	Result            json.RawMessage
	Error             json.RawMessage
	Dispatch          json.RawMessage
	ExpectedVersion   int64
	Observation       *Observation
	PromoteBinding    bool
	MarkDeleted       bool
	DesiredPower      string
	WakePending       *bool
	MaxHeavy          int
	Apply             func(context.Context, *sql.Tx) error
	ReleaseSourceLock bool
	ForkChildren      []ForkChildTransition
}

type ForkChildTransition struct {
	Ordinal       int
	State         string
	ErrorCode     string
	Observation   *Observation
	ResourceProof []rt.ForkDiskEvidence
}

type Event struct {
	ID           string          `json:"id"`
	Seq          int64           `json:"seq"`
	ControlEpoch string          `json:"control_epoch"`
	Type         string          `json:"type"`
	MachineID    string          `json:"machine_id,omitempty"`
	OperationID  string          `json:"operation_id,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

// MutationRequest is the common envelope for a synchronous management change.
type MutationRequest struct {
	PrincipalID        string
	CredentialID       string
	PrincipalScope     json.RawMessage
	Epoch              string
	Key                string
	Scope              string
	Request            json.RawMessage
	ExpectedRevision   int64
	ExpectedGeneration int64
}

type MutationResult struct {
	Response      json.RawMessage
	RetryResponse json.RawMessage
}
