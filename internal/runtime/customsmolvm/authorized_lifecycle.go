package customsmolvm

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	rt "sandbox-platform/internal/runtime"
)

const (
	managedAcceptance = "metadata/managed-acceptance.json"
	lifecycleLog      = "metadata/managed/system-lifecycle.log"
)

type releaseArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (r Route) readReleaseArtifact(artifacts []releaseArtifact, name string, limit int64) ([]byte, string, error) {
	var entry releaseArtifact
	for _, artifact := range artifacts {
		if artifact.Path == name {
			if entry.Path != "" {
				return nil, "", fmt.Errorf("duplicate release artifact %s", name)
			}
			entry = artifact
		}
	}
	digest := "sha256:" + entry.SHA256
	if entry.Size <= 0 || entry.Size > limit || !validDigest(digest) {
		return nil, "", fmt.Errorf("missing or invalid release artifact %s", name)
	}
	f, err := os.Open(filepath.Join(filepath.Dir(r.Manifest), name))
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, entry.Size+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) != entry.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		return nil, "", fmt.Errorf("release artifact %s differs from the pinned manifest", name)
	}
	return data, digest, nil
}

// The operator-pinned Bundle authenticates the receipt and the raw result.
// This extension leaves the runtime reader's core /4 cases intact.
func (r Route) readAuthorizedLifecycle(raw []byte) (bool, error) {
	var manifest struct {
		Artifacts []releaseArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, err
	}
	data, _, err := r.readReleaseArtifact(manifest.Artifacts, managedAcceptance, 64<<10)
	if err != nil {
		return false, err
	}
	var evidence struct {
		Schema      string `json:"schema"`
		Measurement string `json:"measurement_bundle_digest"`
		Runs        map[string]struct {
			Harness string `json:"harness_sha256"`
		} `json:"runs"`
		Lifecycle *struct {
			Schema  string `json:"schema"`
			Log     string `json:"log_sha256"`
			Harness string `json:"harness_sha256"`
		} `json:"authorized_lifecycle"`
	}
	if err = json.Unmarshal(data, &evidence); err != nil {
		return false, err
	}
	if evidence.Schema != "RuntimeManagedAcceptance/4" || !validDigest(evidence.Measurement) {
		return false, fmt.Errorf("invalid authorized release evidence")
	}
	group := evidence.Lifecycle
	if group == nil {
		return false, nil
	}
	if group.Schema != "RuntimeAuthorizedLifecycle/1" || !validDigest(group.Harness) ||
		group.Harness != evidence.Runs["system-boot"].Harness {
		return false, fmt.Errorf("invalid authorized lifecycle evidence")
	}
	log, digest, err := r.readReleaseArtifact(manifest.Artifacts, lifecycleLog, 4<<20)
	if err != nil {
		return false, err
	}
	if digest != group.Log {
		return false, fmt.Errorf("authorized lifecycle log differs from the receipt")
	}
	log = bytes.TrimSpace(log)
	final := log[bytes.LastIndexByte(log, '\n')+1:]
	witness, found := bytes.CutPrefix(final, []byte("Authorized KVM "))
	var result struct {
		Case        string          `json:"case"`
		Bundle      string          `json:"bundle_digest"`
		Execution   rt.ExecutionRef `json:"execution"`
		Standby     rt.ExecutionRef `json:"standby"`
		Resumed     rt.ExecutionRef `json:"resumed"`
		ColdStarted rt.ExecutionRef `json:"cold_started"`
		Protected   int64           `json:"protected_revision"`
		Cleared     int64           `json:"cleared_revision"`
		Wake        string          `json:"wake_operation_id"`
		Lease       string          `json:"released_lease_id"`
		Stop        string          `json:"stop_operation_id"`
		Start       string          `json:"start_operation_id"`
	}
	if !found || json.Unmarshal(witness, &result) != nil || result.Case != "system-lifecycle" ||
		result.Bundle != evidence.Measurement || !identifier.MatchString(result.Execution.InstanceID) ||
		!identifier.MatchString(result.Execution.IncarnationID) || result.Execution.ExecutionEpoch < 1 ||
		result.Execution != result.Standby || result.Execution != result.Resumed ||
		result.Protected < 1 || result.Cleared <= result.Protected || result.Cleared-result.Protected != 1 ||
		!identifier.MatchString(result.Wake) || !identifier.MatchString(result.Lease) ||
		result.ColdStarted.InstanceID != result.Execution.InstanceID ||
		!identifier.MatchString(result.ColdStarted.IncarnationID) || result.ColdStarted.IncarnationID == result.Execution.IncarnationID ||
		result.ColdStarted.ExecutionEpoch <= result.Execution.ExecutionEpoch ||
		result.ColdStarted.ExecutionEpoch-result.Execution.ExecutionEpoch != 1 ||
		!identifier.MatchString(result.Stop) || !identifier.MatchString(result.Start) || result.Stop == result.Start {
		return false, fmt.Errorf("authorized lifecycle log lacks completed Standby resume and cold restart")
	}
	return true, nil
}
