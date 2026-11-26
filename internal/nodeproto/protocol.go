package nodeproto

import (
	"errors"
	"regexp"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const Version = "NodeGateway/1"
const Prefix = "/node/v1"

const (
	HandshakePath         = Prefix + "/handshake"
	ReconcileKnownPath    = Prefix + "/reconcile-known"
	HealthPath            = Prefix + "/runtime/health"
	ReadyPath             = Prefix + "/runtime/ready"
	ObservePath           = Prefix + "/runtime/observe"
	CapacityPath          = Prefix + "/runtime/capacity"
	DiscoverExecutionPath = Prefix + "/execution/discover"
	DiscoverBindingPath   = Prefix + "/binding/discover"
	InventoryPath         = Prefix + "/execution/inventory"
	QueryOperationPath    = Prefix + "/operations/query"
	SubmitLaunchPath      = Prefix + "/operations/launch"
	SubmitPowerPath       = Prefix + "/operations/power"
	SubmitStatePointPath  = Prefix + "/operations/statepoint"
	SubmitForkPath        = Prefix + "/operations/fork"
	RetryForkPath         = Prefix + "/operations/fork/retry"
	CancelForkPath        = Prefix + "/operations/fork/cancel"
	BootCapabilityPath    = Prefix + "/capabilities/system-launch"
	StatePointCapPath     = Prefix + "/capabilities/statepoint"
	GuestPingPath         = Prefix + "/guest/ping"
	GuestExecPath         = Prefix + "/guest/exec"
	GuestUploadPath       = Prefix + "/guest/upload"
	ServiceStreamPath     = Prefix + "/streams/service"
)

const (
	CodeInvalid       = "INVALID_REQUEST"
	CodeAuthority     = "AUTHORITY_MISMATCH"
	CodeRoute         = "ROUTE_MISMATCH"
	CodeConflict      = "IDEMPOTENCY_CONFLICT"
	CodeUnknown       = "OUTCOME_UNKNOWN"
	CodeUnavailable   = "NODE_UNAVAILABLE"
	CodeUnsupported   = "NOT_SUPPORTED"
	CodeNotFound      = "NOT_FOUND"
	CodeBusy          = "NODE_BUSY"
	CodeForbidden     = "FORBIDDEN"
	CodeNativeRefused = "NATIVE_REFUSED"
)

const (
	ControlRequestLimit = 1 << 20
	ControlReplyLimit   = 4 << 20
	ControlSlots        = 32
	GuestSlots          = 8
	StreamSlots         = 32
	StreamPerExecution  = 4
	FrameSize           = 32 << 10
)

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func ValidID(value string) bool     { return idPattern.MatchString(value) }
func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

type Authority struct {
	WorkerID             string `json:"worker_id" yaml:"worker_id"`
	EnrollmentGeneration uint64 `json:"enrollment_generation" yaml:"enrollment_generation"`
	ControllerID         string `json:"controller_id" yaml:"controller_id"`
	ControlEpoch         string `json:"control_epoch" yaml:"control_epoch"`
}

func (a Authority) Valid() bool {
	return ValidID(a.WorkerID) && a.EnrollmentGeneration > 0 && ValidID(a.ControllerID) && ValidID(a.ControlEpoch)
}

type Route struct {
	RuntimeInstanceID string `json:"runtime_instance_id" yaml:"runtime_instance_id"`
	LocalInstanceID   string `json:"local_instance_id" yaml:"local_instance_id"`
	BundleDigest      string `json:"bundle_digest" yaml:"bundle_digest"`
}

func (r Route) Valid() bool {
	return ValidID(r.RuntimeInstanceID) && ValidID(r.LocalInstanceID) && ValidDigest(r.BundleDigest)
}

type Envelope[T any] struct {
	Protocol  string    `json:"protocol"`
	Authority Authority `json:"authority"`
	Route     Route     `json:"route"`
	Request   T         `json:"request"`
}

type Handshake struct {
	Protocol    string      `json:"protocol"`
	Authority   Authority   `json:"authority"`
	NodeSession string      `json:"node_session"`
	HostBootID  string      `json:"host_boot_id"`
	Routes      []RouteInfo `json:"routes"`
	Coverage    string      `json:"coverage"`
}

const KnownOnly = "known_only"

type RouteInfo struct {
	Route
	NativeStop      bool     `json:"native_stop"`
	NativeStandby   bool     `json:"native_standby"`
	ServiceDial     bool     `json:"service_dial"`
	AdmittedActions []string `json:"admitted_actions,omitempty"`
	GuestRelay      bool     `json:"guest_relay,omitempty"`
}

type Empty struct{}

type Name struct {
	Name string `json:"name"`
}

type Operation struct {
	OperationID string `json:"operation_id"`
}

type Kind struct {
	Kind string `json:"kind"`
}

type ForkControl struct {
	OperationID string         `json:"operation_id"`
	Control     rt.ForkControl `json:"control"`
}

type SystemLaunchCapabilities struct {
	Boot   rt.Capability `json:"boot"`
	Growth rt.Capability `json:"growth"`
}

type Inventory struct {
	Native       rt.NativeInventory       `json:"native"`
	Capabilities map[string]rt.Capability `json:"capabilities"`
}

type ReconcileRequest struct {
	Operations []string `json:"operations"`
	Bindings   []string `json:"bindings"`
}

const ReconcileLimit = 128

type KnownOperation struct {
	OperationID string               `json:"operation_id"`
	Receipt     *rt.OperationReceipt `json:"receipt,omitempty"`
	Issue       string               `json:"issue,omitempty"`
}

type KnownBinding struct {
	Name    string            `json:"name"`
	Binding *rt.LaunchBinding `json:"binding,omitempty"`
	Issue   string            `json:"issue,omitempty"`
}

type ReconcileResult struct {
	Coverage       string           `json:"coverage"`
	Operations     []KnownOperation `json:"operations"`
	Bindings       []KnownBinding   `json:"bindings"`
	JournalLimited bool             `json:"journal_limited"`
}

type GuestExec struct {
	Expected rt.ExecutionRef `json:"expected"`
	Exec     rt.ExecRequest  `json:"exec"`
}

type GuestUpload struct {
	Expected rt.ExecutionRef `json:"expected"`
	Path     string          `json:"guest_path"`
	Options  rt.FileOptions  `json:"options"`
	Contents []byte          `json:"contents"`
}

type ServiceStream struct {
	Expected   rt.ExecutionRef                 `json:"expected"`
	Port       uint16                          `json:"port"`
	Attachment *workerclient.NetworkAttachment `json:"attachment,omitempty"`
}

func ValidExecution(ref rt.ExecutionRef) bool {
	return ValidID(ref.InstanceID) && ValidID(ref.IncarnationID) && ref.ExecutionEpoch > 0
}

// Failure contains a safe machine-readable failure at the network boundary.
type Failure struct {
	Code       string `json:"code"`
	NativeCode string `json:"native_code,omitempty"`
}

func (f *Failure) Error() string { return "node gateway: " + f.Code }

func (f *Failure) Unwrap() error {
	code, native := "", ""
	switch f.Code {
	case CodeNotFound:
		code, native = "NOT_FOUND", "E_NOT_FOUND"
	case CodeGateClosed:
		code, native = "CONFLICT", "E_STATE_CONFLICT"
	case CodeInvalid:
		code, native = "INVALID_ARGUMENT", "E_INVALID_REQUEST"
	case CodeUnsupported:
		code, native = "NOT_SUPPORTED", "E_CAPABILITY_UNSUPPORTED"
	case CodeNativeRefused:
		if NativeRefusal(f.NativeCode) {
			code, native = "NATIVE_REFUSED", f.NativeCode
		}
	}
	if native == "" {
		return nil
	}
	return &rt.Error{Code: code, NativeCode: native, Message: f.Error()}
}

func NativeRefusal(code string) bool {
	switch code {
	case "E_CAPABILITY_UNSUPPORTED", "E_NATIVE_UNBOUND", "E_STALE_EXECUTION", "E_STATE_CONFLICT",
		"E_VOLUME_CONFLICT", "E_OPERATION_IN_PROGRESS", "E_PERMISSION_DENIED", "E_INVALID_REQUEST",
		"E_VOLUME_GENERATION_MISMATCH", "E_STATEPOINT_CORRUPT", "E_BUNDLE_UNAVAILABLE", "E_FENCE_UNPROVEN",
		"E_RESOURCE_UNAVAILABLE", "E_RESOURCE_LIMIT", "E_VOLUME_REQUIRES_STOPPED",
		"E_VOLUME_STORAGE_NOT_APPLIED", "E_VOLUME_BUSY", "E_STALE_REVISION", "E_NOT_FOUND":
		return true
	default:
		return false
	}
}

func (f *Failure) Is(target error) bool {
	switch f.Code {
	case CodeInvalid:
		return target == rt.ErrInvalid
	case CodeConflict:
		return target == rt.ErrConflict
	case CodeNotFound:
		return target == rt.ErrNotFound
	case CodeUnsupported, CodeRoute:
		return target == rt.ErrUnsupported
	case CodeUnknown:
		return target == rt.ErrUncertain || target == rt.ErrUnavailable
	case CodeUnavailable, CodeBusy, CodeAuthority:
		return target == rt.ErrUnavailable
	case CodeGateClosed:
		return target == rt.ErrConflict
	case CodeNativeRefused:
		switch f.NativeCode {
		case "E_VOLUME_REQUIRES_STOPPED":
			return target == rt.ErrVolumeRequiresStopped || target == rt.ErrConflict
		case "E_STALE_REVISION", "E_STALE_EXECUTION", "E_STATE_CONFLICT", "E_VOLUME_CONFLICT", "E_VOLUME_BUSY":
			return target == rt.ErrConflict
		case "E_NOT_FOUND":
			return target == rt.ErrNotFound
		}
		return false
	default:
		return false
	}
}

func FailureCode(err error) string {
	var failure *Failure
	switch {
	case errors.As(err, &failure):
		return failure.Code
	case errors.Is(err, rt.ErrInvalid):
		return CodeInvalid
	case errors.Is(err, rt.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, rt.ErrConflict):
		return CodeConflict
	case errors.Is(err, rt.ErrUnsupported):
		return CodeUnsupported
	default:
		return CodeUnavailable
	}
}

const (
	GuestAgentStreamPath = Prefix + "/streams/guest-agent"
	GatePath             = Prefix + "/maintenance/gate"
	SetGatePath          = Prefix + "/maintenance/set-gate"
	GateActive           = "active"
	GateCordoned         = "cordoned"
	GateDraining         = "draining"
	CodeGateClosed       = "REMOTE_GATE_CLOSED"
)

type Gate struct {
	Mode     string `json:"mode"`
	Revision uint64 `json:"revision"`
	Coverage string `json:"coverage"`
}

type SetGate struct {
	Mode             string `json:"mode"`
	ExpectedRevision uint64 `json:"expected_revision"`
}
