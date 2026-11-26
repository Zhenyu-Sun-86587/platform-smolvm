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

type authorizedRelease struct {
	lifecycle  bool
	taskDelete bool
	rollback   bool
}

type authorizedRun struct {
	Log     string `json:"log_sha256"`
	Harness string `json:"harness_sha256"`
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
func (r Route) readAuthorizedRelease(raw []byte) (authorizedRelease, error) {
	var release authorizedRelease
	var manifest struct {
		Artifacts []releaseArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return release, err
	}
	data, _, err := r.readReleaseArtifact(manifest.Artifacts, managedAcceptance, 64<<10)
	if err != nil {
		return release, err
	}
	var evidence struct {
		Schema      string `json:"schema"`
		Measurement string `json:"measurement_bundle_digest"`
		Runs        map[string]struct {
			Harness string `json:"harness_sha256"`
		} `json:"runs"`
		Lifecycle *struct {
			Schema string `json:"schema"`
			authorizedRun
		} `json:"authorized_lifecycle"`
		Owner *struct {
			Schema string                   `json:"schema"`
			Runs   map[string]authorizedRun `json:"runs"`
		} `json:"authorized_owner"`
	}
	if err = json.Unmarshal(data, &evidence); err != nil {
		return release, err
	}
	if evidence.Schema != "RuntimeManagedAcceptance/4" || !validDigest(evidence.Measurement) {
		return release, fmt.Errorf("invalid authorized release evidence")
	}
	read := func(name, path string, run authorizedRun, result any) error {
		if !validDigest(run.Harness) || run.Harness != evidence.Runs["system-boot"].Harness {
			return fmt.Errorf("invalid authorized %s scope", name)
		}
		log, digest, err := r.readReleaseArtifact(manifest.Artifacts, path, 4<<20)
		if err != nil {
			return err
		}
		if digest != run.Log {
			return fmt.Errorf("authorized %s log differs from the receipt", name)
		}
		log = bytes.TrimSpace(log)
		final := log[bytes.LastIndexByte(log, '\n')+1:]
		witness, found := bytes.CutPrefix(final, []byte("Authorized KVM "))
		var bound struct {
			Case   string `json:"case"`
			Bundle string `json:"bundle_digest"`
		}
		if !found || json.Unmarshal(witness, &bound) != nil || bound.Case != name || bound.Bundle != evidence.Measurement {
			return fmt.Errorf("invalid final authorized %s result", name)
		}
		return json.Unmarshal(witness, result)
	}
	if group := evidence.Lifecycle; group != nil {
		var result lifecycleWitness
		if group.Schema != "RuntimeAuthorizedLifecycle/1" {
			return release, fmt.Errorf("invalid authorized lifecycle evidence")
		}
		if err := read("system-lifecycle", lifecycleLog, group.authorizedRun, &result); err != nil {
			return release, err
		}
		if !result.valid() {
			return release, fmt.Errorf("authorized lifecycle log lacks completed Standby resume and cold restart")
		}
		release.lifecycle = true
	}
	if group := evidence.Owner; group != nil {
		if group.Schema != "RuntimeAuthorizedOwner/1" || len(group.Runs) == 0 {
			return release, fmt.Errorf("invalid authorized owner evidence")
		}
		for name, run := range group.Runs {
			switch name {
			case "system-tasks-delete":
				var result struct {
					Original  rt.ExecutionRef `json:"original"`
					Execution rt.ExecutionRef `json:"execution"`
					Task      string          `json:"task_id"`
					Capture   string          `json:"capture_id"`
					Create    string          `json:"task_operation_id"`
					Cancel    string          `json:"cancel_operation_id"`
					Stop      string          `json:"stop_operation_id"`
					Delete    string          `json:"delete_operation_id"`
					Marker    string          `json:"marker"`
				}
				if err := read(name, "metadata/managed/system-tasks-delete.log", run, &result); err != nil {
					return release, err
				}
				if !release.lifecycle || !nextReleaseExecution(result.Original, result.Execution) ||
					!validReleaseIDs(result.Task, result.Capture, result.Create, result.Cancel, result.Stop, result.Delete, result.Marker) {
					return release, fmt.Errorf("authorized owner log lacks task and retirement results")
				}
				release.taskDelete = true
			case "system-rollback":
				var result struct {
					Original rt.ExecutionRef `json:"original"`
					Restored rt.ExecutionRef `json:"restored"`
					Snapshot string          `json:"snapshot_id"`
					Rescue   string          `json:"rescue_capture_id"`
					Op       string          `json:"operation_id"`
					Marker   string          `json:"marker"`
				}
				if err := read(name, "metadata/managed/system-rollback.log", run, &result); err != nil {
					return release, err
				}
				if evidence.Runs["system-restore"].Harness != run.Harness ||
					!nextReleaseExecution(result.Original, result.Restored) ||
					!validReleaseIDs(result.Snapshot, result.Rescue, result.Op, result.Marker) {
					return release, fmt.Errorf("authorized owner log lacks a same-machine rollback result")
				}
				release.rollback = true
			default:
				return release, fmt.Errorf("unsupported authorized owner case %q", name)
			}
		}
	}
	return release, nil
}

type lifecycleWitness struct {
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

func (result lifecycleWitness) valid() bool {
	return result.Execution == result.Standby && result.Execution == result.Resumed &&
		result.Protected > 0 && result.Cleared > result.Protected && result.Cleared-result.Protected == 1 &&
		validReleaseIDs(result.Wake, result.Lease, result.Stop, result.Start) && result.Stop != result.Start &&
		nextReleaseExecution(result.Execution, result.ColdStarted)
}

func nextReleaseExecution(old, next rt.ExecutionRef) bool {
	return validReleaseIDs(old.InstanceID, old.IncarnationID, next.IncarnationID) && old.ExecutionEpoch > 0 &&
		old.InstanceID == next.InstanceID && old.IncarnationID != next.IncarnationID &&
		next.ExecutionEpoch > old.ExecutionEpoch && next.ExecutionEpoch-old.ExecutionEpoch == 1
}

func validReleaseIDs(values ...string) bool {
	for _, value := range values {
		if !identifier.MatchString(value) {
			return false
		}
	}
	return true
}
