package snapshots

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const captureConfigurationVersion = 1

// CaptureConfiguration records the platform machine configuration at dispatch.
// The Worker remains the authority for the state and resources restored from the cut.
type CaptureConfiguration struct {
	Status                string          `json:"status"`
	Source                string          `json:"source,omitempty"`
	SchemaVersion         int             `json:"schema_version,omitempty"`
	SourceMachineRevision int64           `json:"source_machine_revision,omitempty"`
	TemplateID            string          `json:"template_id,omitempty"`
	CaptureID             string          `json:"capture_id,omitempty"`
	ManifestDigest        string          `json:"manifest_digest,omitempty"`
	SpecDigest            string          `json:"spec_digest,omitempty"`
	Spec                  json.RawMessage `json:"spec,omitempty"`
	ReasonCode            string          `json:"reason_code,omitempty"`
}

type CaptureDisk struct {
	DeviceID      string `json:"device_id"`
	Role          string `json:"role"`
	VolumeID      string `json:"volume_id"`
	GenerationID  string `json:"generation_id"`
	ReadOnly      bool   `json:"read_only"`
	CapacityBytes uint64 `json:"capacity_bytes"`
}

type CapturedScope struct {
	CaptureID      string        `json:"capture_id"`
	ManifestDigest string        `json:"manifest_digest"`
	RAMBytes       uint64        `json:"ram_bytes"`
	Disks          []CaptureDisk `json:"disks,omitempty"`
}

type CaptureSummary struct {
	CaptureMode     CaptureMode              `json:"capture_mode"`
	Configuration   CaptureConfiguration     `json:"configuration"`
	RAM             string                   `json:"ram"`
	RAMBytes        *uint64                  `json:"ram_bytes,omitempty"`
	Disks           []CaptureDisk            `json:"disks,omitempty"`
	RestorePolicies []rt.VolumeRestorePolicy `json:"restore_policies,omitempty"`
	VerifiedAt      *time.Time               `json:"verified_at,omitempty"`
	ReasonCode      string                   `json:"reason_code,omitempty"`
}

type captureMetadata struct {
	Configuration *CaptureConfiguration `json:"captured_configuration,omitempty"`
	Scope         *CapturedScope        `json:"captured_scope,omitempty"`
}

type machineCaptureSpec struct {
	TemplateID        string             `json:"template_id"`
	BootMode          rt.BootMode        `json:"boot_mode,omitempty"`
	MemoryForkProfile string             `json:"memory_fork_profile,omitempty"`
	LiveComputeGrowth bool               `json:"live_compute_growth,omitempty"`
	Network           *rt.LaunchNetwork  `json:"network,omitempty"`
	Resources         *capturedResources `json:"resources,omitempty"`
}

type capturedResources struct {
	CPUs              *uint8  `json:"cpu_count,omitempty"`
	Memory            *uint32 `json:"memory_mib,omitempty"`
	Storage           *uint64 `json:"storage_gib,omitempty"`
	Overlay           *uint64 `json:"overlay_gib,omitempty"`
	CPULimitMillicpus *uint32 `json:"cpu_limit_millicpus,omitempty"`
	BalloonTargetMiB  *uint32 `json:"balloon_target_mib,omitempty"`
}

// Take only VM settings. A create request can also contain names or lifecycle
// intent. The restored machine gets its own identity and authorization.
func CaptureSpec(raw json.RawMessage, templateID string, revision int64, captureID string) (*CaptureConfiguration, error) {
	var spec machineCaptureSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	spec.TemplateID = templateID
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return &CaptureConfiguration{Status: "verified", Source: "machine_spec", SchemaVersion: captureConfigurationVersion,
		SourceMachineRevision: revision, TemplateID: templateID, CaptureID: captureID, Spec: encoded,
		SpecDigest: captureSpecDigest(encoded)}, nil
}

// RestoreMachineSpec projects the captured machine settings used by the
// platform after restore. The saved policy targets remain capture evidence.
// The active execution reports its CPU quota and balloon state independently.
func (c *CaptureConfiguration) RestoreMachineSpec() json.RawMessage {
	var spec machineCaptureSpec
	if json.Unmarshal(c.Spec, &spec) != nil {
		return nil
	}
	if spec.Resources != nil {
		spec.Resources.CPULimitMillicpus = nil
		spec.Resources.BalloonTargetMiB = nil
	}
	encoded, _ := json.Marshal(spec)
	return encoded
}

func captureSpecDigest(spec json.RawMessage) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(spec))
}

func captureMetadataFor(s Snapshot) captureMetadata {
	var metadata captureMetadata
	if s.VM == nil || json.Unmarshal(s.Manifest, &metadata) != nil {
		return captureMetadata{}
	}
	return metadata
}

func CapturedConfiguration(s Snapshot) *CaptureConfiguration {
	return verifiedCaptureConfiguration(s, captureMetadataFor(s).Configuration)
}

func verifiedCaptureConfiguration(s Snapshot, config *CaptureConfiguration) *CaptureConfiguration {
	if config == nil || config.Status != "verified" || config.SourceMachineRevision < 1 || config.TemplateID == "" ||
		config.SchemaVersion != captureConfigurationVersion || config.CaptureID != s.VM.RuntimeCaptureID ||
		config.ManifestDigest != s.Hash || config.ManifestDigest == "" {
		return nil
	}
	// Canonicalize again so insignificant whitespace and key ordering are not
	// part of the proof, and never replay unrecognized fields into target spec.
	canonical, err := CaptureSpec(config.Spec, config.TemplateID, config.SourceMachineRevision, config.CaptureID)
	if err != nil || canonical.SpecDigest != config.SpecDigest {
		return nil
	}
	config.Spec = canonical.Spec
	return config
}

func SummarizeCapture(s Snapshot) CaptureSummary {
	summary := CaptureSummary{CaptureMode: s.CaptureMode, RAM: "unknown", ReasonCode: "CAPTURE_SCOPE_UNRECORDED",
		Configuration: CaptureConfiguration{Status: "unknown", ReasonCode: "CAPTURE_CONFIGURATION_UNRECORDED"}}
	if s.VM == nil {
		return summary
	}
	metadata := captureMetadataFor(s)
	if config := verifiedCaptureConfiguration(s, metadata.Configuration); config != nil {
		summary.Configuration = *config
	} else if metadata.Configuration != nil {
		summary.Configuration.ReasonCode = "CAPTURE_CONFIGURATION_UNVERIFIED"
	}
	summary.VerifiedAt = s.Closure
	if s.State == "ready" && s.Closure != nil {
		summary.RestorePolicies = []rt.VolumeRestorePolicy{rt.RestoreCloneConflicts, rt.RestoreExact}
	}
	if scope := metadata.Scope; scope != nil && scope.CaptureID == s.VM.RuntimeCaptureID && scope.ManifestDigest == s.Hash && s.Hash != "" {
		if scope.RAMBytes > 0 {
			summary.RAM, summary.RAMBytes = "included", &scope.RAMBytes
		}
		summary.Disks = scope.Disks
		if summary.RAMBytes != nil && len(summary.Disks) != 0 {
			summary.ReasonCode = ""
		}
	}
	return summary
}
