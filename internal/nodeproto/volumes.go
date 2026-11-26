package nodeproto

import rt "sandbox-platform/internal/runtime"

const (
	VolumeConsumerPath = Prefix + "/volume/consumer"
	VolumeObservePath  = Prefix + "/volume/observe"
	VolumeApplyPath    = Prefix + "/volume/apply"
	StorageSlots       = 2
)

type VolumeLookup struct {
	VolumeID string `json:"volume_id"`
	OwnerID  string `json:"owner_id"`
}

type VolumeMutation struct {
	OperationID      string        `json:"operation_id"`
	RequestDigest    string        `json:"request_digest"`
	VolumeID         string        `json:"volume_id"`
	OwnerID          string        `json:"owner_id"`
	ExpectedRevision int64         `json:"expected_revision"`
	Command          VolumeCommand `json:"command"`
}

// VolumeCommand carries catalog IDs and product choices only. The local
// runtime chooses backing files, constructs descriptors and fences writers.
type VolumeCommand struct {
	Action           string `json:"action"`
	CapacityBytes    int64  `json:"capacity_bytes,omitempty"`
	Lifecycle        string `json:"lifecycle,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`
	SnapshotID       string `json:"snapshot_id,omitempty"`
	AttachmentID     string `json:"attachment_id,omitempty"`
	InstanceID       string `json:"instance_id,omitempty"`
	ExecutionEpoch   *int64 `json:"execution_epoch,omitempty"`
	DeviceKey        string `json:"device_key,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ForkPolicy       string `json:"fork_policy,omitempty"`
	CapturePolicy    string `json:"capture_policy,omitempty"`
}

func VolumeMatches(scope Route, target VolumeLookup, v rt.VolumeObservation) bool {
	return v.Schema == "Volume/1" && v.ID == target.VolumeID && v.OwnerID == target.OwnerID &&
		v.RouteID == "deep" && v.BundleDigest == scope.BundleDigest && v.Revision > 0 &&
		(v.State == "available" || v.State == "changing" || v.State == "deleted") &&
		(v.State != "available" || (v.CapacityBytes > 0 && ValidID(v.HeadResourceID)))
}
