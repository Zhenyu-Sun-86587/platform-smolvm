package runtime

import "context"

type VolumeRestorePolicy string

const (
	RestoreExact          VolumeRestorePolicy = "exact"
	RestoreCloneConflicts VolumeRestorePolicy = "clone_conflicts"
)

func ManagedStatePoint(kind string) bool {
	switch kind {
	case VMSnapshot, Hibernate, VMRestore, VMRollback:
		return true
	default:
		return false
	}
}

type StatePointSubmission struct {
	OperationID   string               `json:"operation_id"`
	RequestDigest string               `json:"request_digest"`
	Expected      LaunchBinding        `json:"expected"`
	Kind          string               `json:"kind"`
	Parameters    StatePointParameters `json:"parameters"`
}

type StatePointParameters struct {
	Name      string              `json:"name,omitempty"`
	CaptureID string              `json:"capture_id"`
	Mode      VolumeRestorePolicy `json:"mode,omitempty"`
}

type StatePointProgress struct {
	Stage                     string            `json:"stage"`
	Phase                     string            `json:"phase,omitempty"`
	CaptureID                 string            `json:"capture_id"`
	RepresentationID          string            `json:"representation_id,omitempty"`
	ManifestDigest            string            `json:"manifest_digest,omitempty"`
	TargetOperationID         string            `json:"target_operation_id,omitempty"`
	RescueCaptureID           string            `json:"rescue_capture_id,omitempty"`
	OriginalSourceDisposition string            `json:"original_source_disposition,omitempty"`
	SourceReleased            bool              `json:"source_released,omitempty"`
	VolumeMap                 map[string]string `json:"volume_map,omitempty"`
	ProductAttachments        map[string]string `json:"product_attachments,omitempty"`
	ActivationDecision        string            `json:"activation_decision,omitempty"`
	ErrorCode                 string            `json:"error_code,omitempty"`
	Error                     string            `json:"error,omitempty"`
}

type StatePointControl interface {
	NativeControl
	DiscoverLaunchBinding(context.Context, string) (LaunchBinding, error)
	StatePointCapability(context.Context, string) (Capability, error)
	SubmitStatePoint(context.Context, StatePointSubmission) (OperationReceipt, error)
}
