package platformapi

import (
	"encoding/json"

	"sandbox-platform/internal/guestfiles"
	"sandbox-platform/internal/lifecycle"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) projectFileCapabilities(v *machineView, official bool) {
	const unverified = "GUEST_FILE_TRANSFER_UNVERIFIED"
	available := rt.Capability{Status: "unknown", ReasonCode: unverified}
	transport := "native_agent"
	var readMax int64 = guestfiles.MaxDownload
	if b := v.Binding; b != nil {
		var evidence struct {
			FileTransfer *rt.GuestFileEvidence `json:"file_transfer"`
		}
		switch {
		case official:
			transport = "official_runtime"
			readMax = 0
			available = rt.Capability{Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: "official_runtime"}
		case v.BootMode != rt.BootSystem:
			available = rt.Capability{Status: "unsupported", ReasonCode: "GUEST_FILE_TRANSFER_UNSUPPORTED"}
		case json.Unmarshal(b.GuestManagementEvidence, &evidence) == nil && evidence.FileTransfer != nil && evidence.FileTransfer.Execution == b.ExecutionRef():
			if evidence.FileTransfer.Supported {
				available = rt.Capability{Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: rt.SystemFileTransferCapability}
			} else {
				available = rt.Capability{Status: "unsupported", ReasonCode: "GUEST_FILE_TRANSFER_UNSUPPORTED"}
			}
		}
	}
	for key, limit := range map[string]int64{
		rt.FileListCapability: 0, rt.FileReadCapability: readMax, rt.FileWriteCapability: rt.PublicFileUploadLimit,
		rt.FilePreviewCapability: rt.FileTextPreviewLimit, rt.FileEditCapability: rt.FileTextPreviewLimit,
	} {
		capability := available
		if available.Supported {
			capability.Constraints = lifecycle.JSON(struct {
				Transport string `json:"transport"`
				MaxBytes  int64  `json:"max_bytes,omitempty"`
			}{transport, limit})
		}
		v.Capabilities[key] = capability
	}
	if official && available.Supported && v.GuestStatus != "ready" {
		v.Capabilities[rt.FileListCapability] = rt.Capability{Status: "unknown", ReasonCode: unverified}
	}
}
