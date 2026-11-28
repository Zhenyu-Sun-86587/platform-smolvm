package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

func placementEvidenceMissing(route rt.BundleRoute, reason string) error {
	return &placement.Rejections{Candidates: []placement.Rejection{{
		WorkerID: route.Owner(), RuntimeInstanceID: route.InstanceID, Reason: reason,
	}}}
}

// Manual Restore reads the immutable capture and resource evidence inside the
// same transaction that pins the target and records the reservation.
func statePointRestorePlacement(ctx context.Context, tx *sql.Tx, route rt.BundleRoute, request json.RawMessage, heavy bool) (*placement.Request, error) {
	missing := placementEvidenceMissing(route, "statepoint_resource_evidence_unavailable")
	var capture, digest, reader, manifest string
	err := tx.QueryRowContext(ctx, `SELECT p.runtime_capture_id, s.sha256, p.state_reader_id, s.manifest_json
		FROM vm_snapshot_provenance p JOIN snapshots s ON s.id=p.snapshot_id
		JOIN managed_statepoint_receipts c ON c.runtime_instance_id=p.runtime_instance_id AND c.capture_id=p.runtime_capture_id
			AND c.runtime_bundle_id=p.runtime_bundle_id AND c.operation_id=p.operation_id AND c.manifest_digest=s.sha256
		WHERE p.snapshot_id=json_extract(?,'$.snapshot_id') AND p.runtime_instance_id=? AND p.runtime_bundle_id=?
		AND s.state='ready' AND s.closure_verified_at IS NOT NULL AND c.purpose='checkpoint'`,
		string(request), route.InstanceID, route.BundleID).Scan(&capture, &digest, &reader, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	var evidence struct {
		Configuration struct {
			Status                string          `json:"status"`
			SchemaVersion         int             `json:"schema_version"`
			SourceMachineRevision int64           `json:"source_machine_revision"`
			CaptureID             string          `json:"capture_id"`
			ManifestDigest        string          `json:"manifest_digest"`
			SpecDigest            string          `json:"spec_digest"`
			Spec                  json.RawMessage `json:"spec"`
		} `json:"captured_configuration"`
		Scope struct {
			CaptureID      string `json:"capture_id"`
			ManifestDigest string `json:"manifest_digest"`
			Disks          []struct {
				ID           string `json:"device_id"`
				Role         string `json:"role"`
				GenerationID string `json:"generation_id"`
				ReadOnly     *bool  `json:"read_only"`
				Bytes        int64  `json:"capacity_bytes"`
			} `json:"disks"`
		} `json:"captured_scope"`
	}
	if err := json.Unmarshal([]byte(manifest), &evidence); err != nil {
		return nil, missing
	}
	config, scope := evidence.Configuration, evidence.Scope
	var compact bytes.Buffer
	if config.Status != "verified" || config.SchemaVersion != 1 || config.SourceMachineRevision < 1 ||
		config.CaptureID != capture || config.ManifestDigest != digest || scope.CaptureID != capture ||
		scope.ManifestDigest != digest || json.Compact(&compact, config.Spec) != nil ||
		config.SpecDigest != fmt.Sprintf("sha256:%x", sha256.Sum256(compact.Bytes())) {
		return nil, missing
	}
	var capacity int64
	var root bool
	seen := make(map[string]bool, len(scope.Disks))
	for _, disk := range scope.Disks {
		if disk.ID == "" || disk.GenerationID == "" || disk.ReadOnly == nil || disk.Bytes < 1 || seen[disk.ID] {
			return nil, missing
		}
		seen[disk.ID] = true
		if !*disk.ReadOnly {
			if capacity > math.MaxInt64-disk.Bytes {
				return nil, missing
			}
			capacity += disk.Bytes
			root = root || disk.Role == "root"
		}
	}
	if !root {
		return nil, missing
	}
	result, err := ownerPlacement(config.Spec, route, route.Owner(), rt.FullStateOperationCapability(rt.VMRestore), 1, heavy)
	if err != nil {
		return nil, missing
	}
	result.Reader, result.Resources.StorageBytes = reader, capacity
	return result, nil
}
