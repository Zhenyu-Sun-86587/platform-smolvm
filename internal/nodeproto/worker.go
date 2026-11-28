package nodeproto

import (
	"encoding/json"
	"sandbox-platform/internal/runtime/workerclient"
)

const WorkerPath = "/node/v1/worker/"

// The URL selects the operation. A method or a local endpoint supplied in the
// payload cannot change which resourced call will execute.
var LifecycleActions = []string{
	"QueryLifecycleEvents", "QueryHostActivity", "GetLifecycleStatus", "ApplyLifecyclePolicy",
	"ReportActivity", "AcquireActivityHold", "ReleaseActivityHold", "AcquireRunningLease",
	"RenewRunningLease", "ReleaseRunningLease", "RequestWake", "AuthorizeWakeActivation",
	"CancelWakeDemand", "CloseAccessProtection", "QueryProtection", "RetireStoppedInstance",
}

type PolicyRequest struct {
	Expected    workerclient.Execution `json:"expected"`
	OperationID string                 `json:"operation_id,omitempty"`
	Payload     json.RawMessage        `json:"payload"`
}

type WorkerResult[T any] struct {
	Value T                   `json:"value"`
	Fault *workerclient.Fault `json:"fault,omitempty"`
}

type ResourceChange struct {
	Expected    workerclient.Execution      `json:"expected"`
	OperationID string                      `json:"operation_id"`
	Change      workerclient.ResourceChange `json:"change"`
}
type CPULimit struct {
	Expected    workerclient.Execution `json:"expected"`
	OperationID string                 `json:"operation_id"`
	Millicpus   *uint32                `json:"millicpus"`
}
type PacketRule struct {
	Expected    workerclient.Execution      `json:"expected"`
	Rule        workerclient.PacketWakeRule `json:"rule"`
	OperationID string                      `json:"operation_id,omitempty"`
	Digest      string                      `json:"request_digest,omitempty"`
}
type CaptureRef struct {
	Expected    workerclient.Execution `json:"expected"`
	OperationID string                 `json:"operation_id"`
	Digest      string                 `json:"request_digest"`
	CaptureID   string                 `json:"capture_id"`
}
type ReleaseCapture struct {
	OperationID string `json:"source_operation_id"`
	CaptureID   string `json:"capture_id"`
	Digest      string `json:"manifest_digest"`
}
