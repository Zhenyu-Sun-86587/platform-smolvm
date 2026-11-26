package nodeproto

import "sandbox-platform/internal/placement"

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
}
type SampleRequest struct {
	Nonce   string           `json:"nonce"`
	Routes  []RequestedRoute `json:"routes"`
	Pending []AdmissionQuery `json:"pending,omitempty"`
}
type SampleReply struct {
	Sample     placement.Sample      `json:"sample"`
	Artifacts  map[string][]Artifact `json:"artifacts"`
	Admissions []placement.Admission `json:"admissions,omitempty"`
}
