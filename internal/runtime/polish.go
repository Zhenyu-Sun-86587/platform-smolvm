package runtime

import "time"

const (
	SystemFileTransferCapability = "system-file-transfer-v1"
	SystemExecStreamCapability   = "system-exec-stream-v1"
)

const (
	FileListCapability    = "files.list"
	FileReadCapability    = "files.read"
	FileWriteCapability   = "files.write"
	FilePreviewCapability = "files.preview"
	FileEditCapability    = "files.edit"

	PublicFileUploadLimit   = 64 << 20
	FileTextPreviewLimit    = 256 << 10
	PrivateGuestUploadLimit = 512 << 10
)

type MetricObservation struct {
	Availability string                 `json:"availability"`
	Source       string                 `json:"source"`
	Scope        string                 `json:"scope"`
	ObservedAt   *time.Time             `json:"observed_at,omitempty"`
	Execution    *MetricExecution       `json:"execution,omitempty"`
	Issues       map[string]MetricIssue `json:"issues,omitempty"`
}

type MetricExecution struct {
	BindingID         string `json:"binding_id"`
	BindingGeneration int64  `json:"binding_generation"`
	IncarnationID     string `json:"incarnation_id"`
	ExecutionEpoch    int64  `json:"execution_epoch"`
}

type MetricIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GuestFileEvidence is the transfer handshake for one exact running Guest Agent.
type GuestFileEvidence struct {
	Execution ExecutionRef `json:"execution"`
	Supported bool         `json:"supported"`
}

// GuestExecStreamEvidence is the streaming handshake for one exact Guest execution.
type GuestExecStreamEvidence struct {
	Execution ExecutionRef `json:"execution"`
	Supported bool         `json:"supported"`
}
