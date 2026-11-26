package runtime

import (
	"context"
	"encoding/json"
	"errors"
)

const VolumeOperation = "volume"

var ErrVolumeRequiresStopped = errors.New("volume topology requires a fenced stopped execution")

type VolumeRoute struct {
	RuntimeInstanceID string `json:"runtime_instance_id"`
	RuntimeBundleID   string `json:"runtime_bundle_id"`
}

type VolumeSnapshot struct {
	ID             string `json:"snapshot_id"`
	GenerationID   string `json:"generation_id"`
	SourceRevision int64  `json:"source_revision"`
	CapacityBytes  int64  `json:"capacity_bytes"`
}

type VolumeAttachment struct {
	ID            string `json:"attachment_id"`
	InstanceID    string `json:"instance_id"`
	DeviceKey     string `json:"device_key"`
	Mode          string `json:"mode"`
	GenerationID  string `json:"generation_id,omitempty"`
	ForkPolicy    string `json:"fork_policy"`
	CapturePolicy string `json:"capture_policy"`
	State         string `json:"state"`
	Revision      int64  `json:"revision"`
}

type VolumeObservation struct {
	Schema            string             `json:"schema"`
	ID                string             `json:"volume_id"`
	OwnerID           string             `json:"owner_id"`
	RouteID           string             `json:"route_id"`
	BundleDigest      string             `json:"bundle_digest"`
	Lifecycle         string             `json:"lifecycle"`
	MachineInstanceID string             `json:"machine_instance_id,omitempty"`
	Revision          int64              `json:"revision"`
	State             string             `json:"state"`
	HeadResourceID    string             `json:"head_resource_id,omitempty"`
	GenerationID      string             `json:"generation_id,omitempty"`
	CapacityBytes     int64              `json:"capacity_bytes,omitempty"`
	Snapshots         []VolumeSnapshot   `json:"snapshots"`
	Attachments       []VolumeAttachment `json:"attachments"`
}

type VolumeMutation struct {
	OperationID      string          `json:"operation_id"`
	RequestDigest    string          `json:"request_digest"`
	VolumeID         string          `json:"volume_id"`
	OwnerID          string          `json:"owner_id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Command          json.RawMessage `json:"command"`
}

type VolumeReceipt struct {
	Schema                 string            `json:"schema"`
	OperationID            string            `json:"operation_id"`
	Volume                 VolumeObservation `json:"volume"`
	BlockCapacityApplied   bool              `json:"block_capacity_applied"`
	FilesystemResizeStatus string            `json:"filesystem_resize_status"`
}

type VolumeControl interface {
	ObserveVolume(context.Context, string, string) (VolumeObservation, error)
	ApplyVolume(context.Context, VolumeMutation) (VolumeReceipt, error)
}

type VolumeConsumerResolver interface {
	DiscoverVolumeConsumer(context.Context, string) (ExecutionRef, error)
}
