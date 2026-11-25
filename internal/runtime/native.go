package runtime

import (
	"context"
	"encoding/json"
)

const (
	Wake                   = "wake"
	ManagedStandbyContract = "CaptureControl/1"
)

type ExecutionRef struct {
	InstanceID     string `json:"instance_id"`
	IncarnationID  string `json:"incarnation_id"`
	ExecutionEpoch int64  `json:"execution_epoch"`
}

type NativeInventory struct {
	Execution struct {
		ExecutionRef
		RouteID      string `json:"route_id"`
		BundleDigest string `json:"bundle_digest"`
		Phase        string `json:"phase"`
	} `json:"execution"`
	Inventory struct {
		Protocol          string          `json:"protocol"`
		SourceDisposition string          `json:"source_disposition"`
		MeasuredHelper    bool            `json:"measured_helper"`
		StandbyBlocker    json.RawMessage `json:"standby_blocker"`
		Devices           []NativeDevice  `json:"devices"`
	} `json:"inventory"`
}

type NativeDevice struct {
	VirtioType int              `json:"virtio_type"`
	Tap        *json.RawMessage `json:"tap"`
}

func (i NativeInventory) ManagedStandby() bool {
	if !i.Inventory.MeasuredHelper || i.Inventory.Protocol != ManagedStandbyContract ||
		string(i.Inventory.StandbyBlocker) != "null" || i.Inventory.Devices == nil {
		return false
	}
	for _, device := range i.Inventory.Devices {
		if device.VirtioType == 1 && device.Tap == nil {
			return false
		}
	}
	return true
}

type NativeSubmission struct {
	OperationID   string           `json:"operation_id"`
	RequestDigest string           `json:"request_digest"`
	Expected      ExecutionRef     `json:"expected"`
	Kind          string           `json:"kind"`
	Parameters    NativeParameters `json:"parameters"`
}

type NativeParameters struct {
	Mode string `json:"mode,omitempty"`
}

type NativeProgress struct {
	Stage             string  `json:"stage"`
	Mode              string  `json:"mode"`
	SourceDisposition string  `json:"source_disposition"`
	LastError         *string `json:"last_error"`
}

type OperationReceipt struct {
	OperationID            string          `json:"operation_id"`
	Kind                   string          `json:"kind"`
	RequestDigest          string          `json:"request_digest"`
	RouteID                string          `json:"route_id"`
	InstanceID             string          `json:"instance_id"`
	BundleDigest           string          `json:"bundle_digest"`
	ExpectedExecutionEpoch int64           `json:"expected_execution_epoch"`
	ExpectedIncarnationID  *string         `json:"expected_incarnation_id"`
	Native                 *NativeProgress `json:"native,omitempty"`
	Phase                  string          `json:"phase"`
	Decision               string          `json:"decision"`
	Revision               int64           `json:"revision"`
}

type NativeControl interface {
	DiscoverExecution(context.Context, string) (ExecutionRef, error)
	NativeInventory(context.Context, ExecutionRef) (NativeInventory, error)
	SubmitNative(context.Context, NativeSubmission) (OperationReceipt, error)
	QueryOperation(context.Context, string) (OperationReceipt, error)
}

func NativePowerCapability(kind string) string {
	switch kind {
	case Standby:
		return "lifecycle.standby"
	case Wake:
		return "lifecycle.standby_wake"
	default:
		return ""
	}
}

func NativePowerUnavailable(kind string) error {
	return &CapabilityError{Capability: NativePowerCapability(kind), Status: fullStateCapability(false)}
}
