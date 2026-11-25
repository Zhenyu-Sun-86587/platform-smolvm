package runtime

import "context"

const NativeForkProfile = "managed-parked-v1"
const MaxForkChildren = 32
const ForkAllOrReject = "all_or_reject"
const ForkBestEffort = "best_effort"

func ForkAdmission(value string) string {
	if value == "" {
		return ForkAllOrReject
	}
	return value
}

func IsNativeFork(kind string) bool {
	return kind == IndependentFork || kind == ForkBatch
}

type ForkSubmission struct {
	OperationID   string       `json:"operation_id"`
	RequestDigest string       `json:"request_digest"`
	Expected      ExecutionRef `json:"expected"`
	Kind          string       `json:"kind"`
	Parameters    struct {
		Children  []string `json:"children"`
		Admission string   `json:"admission,omitempty"`
	} `json:"parameters"`
}

type ForkProgress struct {
	Stage                     string             `json:"stage"`
	CaptureID                 string             `json:"capture_id"`
	OriginalSourceDisposition string             `json:"original_source_disposition"`
	SourceReleased            bool               `json:"source_released"`
	Admission                 string             `json:"admission,omitempty"`
	Children                  []ForkChildReceipt `json:"children"`
	Outcome                   string             `json:"outcome"`
	Error                     *string            `json:"error"`
}

type ForkChildReceipt struct {
	Name           string  `json:"name"`
	InstanceID     string  `json:"instance_id"`
	OperationID    string  `json:"operation_id"`
	Phase          string  `json:"phase"`
	IncarnationID  *string `json:"incarnation_id"`
	ExecutionEpoch int64   `json:"execution_epoch"`
	Attempt        int64   `json:"attempt"`
	Cancelled      bool    `json:"cancelled"`
	AdmissionError string  `json:"admission_error,omitempty"`
}

type ForkControl struct {
	RequestDigest    string       `json:"request_digest"`
	Expected         ExecutionRef `json:"expected"`
	ExpectedRevision int64        `json:"expected_revision"`
}

type ForkRetryControl interface {
	RetryFork(context.Context, string, ForkControl) (OperationReceipt, error)
}

type NativeForkControl interface {
	NativeControl
	SubmitFork(context.Context, ForkSubmission) (OperationReceipt, error)
}

type NativeBlock struct {
	ReadOnly          bool `json:"read_only"`
	ManagedAttachment *struct {
		AttachmentID   string `json:"attachment_id"`
		DeviceID       string `json:"device_id"`
		Role           string `json:"role"`
		VolumeID       string `json:"volume_id"`
		GenerationID   string `json:"generation_id"`
		WritableHeadID string `json:"writable_head_id"`
		GrantID        string `json:"grant_id"`
		ExecutionEpoch int64  `json:"execution_epoch"`
	} `json:"managed_attachment"`
}

// ForkDiskEvidence compares independent writers without relying on a live source.
type ForkDiskEvidence struct {
	DeviceID       string `json:"device_id"`
	Role           string `json:"role"`
	VolumeID       string `json:"volume_id"`
	WritableHeadID string `json:"writable_head_id,omitempty"`
	ReadOnly       bool   `json:"read_only"`
}

func (i NativeInventory) ForkDisks() []ForkDiskEvidence {
	var disks []ForkDiskEvidence
	devices := map[string]bool{}
	for _, device := range i.Inventory.Devices {
		if device.VirtioType != 2 {
			continue
		}
		block := device.Block
		if block == nil || block.ManagedAttachment == nil {
			return nil
		}
		a := block.ManagedAttachment
		if a.AttachmentID == "" || a.DeviceID == "" || devices[a.DeviceID] || a.Role == "" || a.VolumeID == "" || a.GenerationID == "" ||
			a.ExecutionEpoch != i.Execution.ExecutionEpoch || block.ReadOnly != (a.WritableHeadID == "" && a.GrantID == "") ||
			(!block.ReadOnly && (a.WritableHeadID == "" || a.GrantID == "")) {
			return nil
		}
		devices[a.DeviceID] = true
		disks = append(disks, ForkDiskEvidence{a.DeviceID, a.Role, a.VolumeID, a.WritableHeadID, block.ReadOnly})
	}
	return disks
}

func VerifiedForkProfile(c Capability) bool {
	return c.Status == "supported" && c.Supported && c.ContractVersion == 1 && c.VerifiedProfile == NativeForkProfile
}

func (i NativeInventory) ForkEligible(kind string) bool {
	for _, device := range i.Inventory.Devices {
		if device.VirtioType == 19 && !device.ParkedVSockRebind {
			return false
		}
	}
	return VerifiedForkProfile(i.Capabilities[FullStateOperationCapability(kind)]) && i.ManagedStandby() &&
		(i.Inventory.SourceDisposition == "RUNNING" || i.Inventory.SourceDisposition == "STANDBY") && len(i.ForkDisks()) > 0
}
