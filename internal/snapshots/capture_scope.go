package snapshots

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"

	rt "sandbox-platform/internal/runtime"
)

// ReadCapturedScope checks the durable Worker record against the independently
// verified completion receipt. The record's disk roots identify sealed points
// even when the attachment was writable at capture time.
func ReadCapturedScope(raw json.RawMessage, request rt.StatePointSubmission, receipt rt.StatePointProgress) *CapturedScope {
	var record struct {
		OperationID    string `json:"operation_id"`
		Phase          string `json:"phase"`
		ManifestDigest string `json:"manifest_digest"`
		Point          struct {
			Schema           string           `json:"schema"`
			CaptureID        string           `json:"capture_id"`
			RepresentationID string           `json:"representation_id"`
			IncarnationID    string           `json:"incarnation_id"`
			ExecutionEpoch   int64            `json:"execution_epoch"`
			Regions          []capturedRegion `json:"regions"`
			Devices          []capturedDevice `json:"devices"`
			DiskRoots        []string         `json:"disk_generations"`
		} `json:"point"`
	}
	if json.Unmarshal(raw, &record) != nil || record.OperationID != request.OperationID || record.Phase != "durable" ||
		record.ManifestDigest == "" || record.ManifestDigest != receipt.ManifestDigest || request.Expected.IncarnationID == nil {
		return nil
	}
	point := record.Point
	if point.Schema != "StatePoint/1" || point.CaptureID != request.Parameters.CaptureID || point.CaptureID != receipt.CaptureID ||
		point.RepresentationID != receipt.RepresentationID || point.IncarnationID != *request.Expected.IncarnationID ||
		point.ExecutionEpoch != request.Expected.ExecutionEpoch {
		return nil
	}
	scope := &CapturedScope{CaptureID: point.CaptureID, ManifestDigest: record.ManifestDigest}
	slices.SortFunc(point.Regions, func(a, b capturedRegion) int { return cmp.Compare(a.GPA, b.GPA) })
	var end, total uint64
	for _, region := range point.Regions {
		if region.Length == 0 || region.GPA < end || region.Length > math.MaxUint64-region.GPA || region.Length > math.MaxUint64-total {
			total = 0
			break
		}
		end, total = region.GPA+region.Length, total+region.Length
	}
	scope.RAMBytes = total
	scope.Disks = projectCapturedDisks(point.Devices, point.DiskRoots, point.ExecutionEpoch)
	return scope
}

type capturedRegion struct {
	GPA    uint64 `json:"gpa"`
	Length uint64 `json:"length"`
}

type capturedDevice struct {
	ID         string `json:"device_id"`
	VirtioType uint32 `json:"virtio_type"`
	Block      *struct {
		CapacityBytes uint64 `json:"capacity_bytes"`
		ReadOnly      *bool  `json:"read_only"`
		Attachment    struct {
			DeviceID       string `json:"device_id"`
			Role           string `json:"role"`
			VolumeID       string `json:"volume_id"`
			GenerationID   string `json:"generation_id"`
			WritableHeadID string `json:"writable_head_id"`
			ExecutionEpoch int64  `json:"execution_epoch"`
			CapacityBytes  uint64 `json:"capacity_bytes"`
		} `json:"managed_attachment"`
	} `json:"block"`
}

func projectCapturedDisks(devices []capturedDevice, roots []string, epoch int64) []CaptureDisk {
	var disks []CaptureDisk
	ids := make(map[string]bool)
	usedRoots := make(map[string]bool)
	for _, device := range devices {
		if device.VirtioType != 2 && device.Block == nil {
			continue
		}
		block := device.Block
		if device.VirtioType != 2 || block == nil || block.ReadOnly == nil || ids[device.ID] {
			return nil
		}
		attachment := block.Attachment
		if device.ID == "" || attachment.DeviceID != device.ID || attachment.ExecutionEpoch != epoch ||
			attachment.Role == "" || attachment.VolumeID == "" || attachment.GenerationID == "" ||
			block.CapacityBytes == 0 || block.CapacityBytes != attachment.CapacityBytes {
			return nil
		}
		generation := attachment.GenerationID
		if !*block.ReadOnly {
			generation = attachment.WritableHeadID
		} else if attachment.WritableHeadID != "" {
			return nil
		}
		if generation == "" || !slices.Contains(roots, generation) {
			return nil
		}
		ids[device.ID], usedRoots[generation] = true, true
		disks = append(disks, CaptureDisk{DeviceID: device.ID, Role: attachment.Role, VolumeID: attachment.VolumeID,
			GenerationID: generation, ReadOnly: *block.ReadOnly, CapacityBytes: block.CapacityBytes})
	}
	if len(usedRoots) != len(roots) {
		return nil
	}
	return disks
}
