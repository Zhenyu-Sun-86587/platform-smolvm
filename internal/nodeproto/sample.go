package nodeproto

import (
	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

const SamplePath = Prefix + "/sample"

type Artifact struct {
	Digest     string `json:"content_digest"`
	Generation string `json:"local_generation_id"`
	Bytes      int64  `json:"capacity_bytes,omitempty"`
}
type RequestedRoute struct {
	Route
	Artifacts []Artifact `json:"artifacts,omitempty"`
}
type AdmissionQuery struct {
	RuntimeInstanceID string `json:"runtime_instance_id"`
	OperationID       string `json:"operation_id"`
	Digest            string `json:"native_digest"`
	Attempt           int64  `json:"attempt"`
	NativeAttempt     int64  `json:"native_attempt"`
}
type AccessWakeQuery struct {
	RuntimeInstanceID string `json:"runtime_instance_id"`
	OperationID       string `json:"operation_id"`
}
type AccessWakeAdmission struct {
	AccessWakeQuery
	Receipt rt.OperationReceipt `json:"receipt"`
}
type SampleRequest struct {
	Nonce       string            `json:"nonce"`
	Routes      []RequestedRoute  `json:"routes"`
	AccessWakes []AccessWakeQuery `json:"access_wakes,omitempty"`
	Pending     []AdmissionQuery  `json:"pending,omitempty"`
}
type SampleReply struct {
	AccessWakes []AccessWakeAdmission `json:"access_wakes,omitempty"`
	Sample      placement.Sample      `json:"sample"`
	Artifacts   map[string][]Artifact `json:"artifacts"`
	Admissions  []placement.Admission `json:"admissions,omitempty"`
}
