package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

type ComputeCapability struct {
	Supported bool    `json:"supported"`
	Available bool    `json:"available_now"`
	Reason    *string `json:"reason"`
}

type ComputeResources struct {
	Execution
	Profile           string                       `json:"profile"`
	CaptureCompatible bool                         `json:"capture_compatible"`
	Capabilities      map[string]ComputeCapability `json:"capabilities"`
	Observed          json.RawMessage              `json:"observed"`
	Pending           json.RawMessage              `json:"pending"`
}

type ResourceChange struct {
	Kind      string  `json:"change"`
	CPUs      uint8   `json:"cpus,omitempty"`
	MemoryMiB uint32  `json:"memory_mib,omitempty"`
	TargetMiB *uint32 `json:"target_mib,omitempty"`
}

type ResourceProgress struct {
	OperationID string          `json:"operation_id"`
	Change      ResourceChange  `json:"change"`
	Stage       string          `json:"stage"`
	Observed    json.RawMessage `json:"observed"`
	ErrorCode   string          `json:"error_code"`
	Error       string          `json:"error"`
}

type ResizeReceipt struct {
	Phase    string            `json:"phase"`
	Decision string            `json:"decision"`
	Resize   *ResourceProgress `json:"resize"`
}

func (c Client) ComputeResources(ctx context.Context, ref Execution) (ComputeResources, error) {
	var profile ComputeResources
	raw, _, err := c.call(ctx, request{Method: "QueryNativeResources", Expected: &ref, Payload: struct{}{}}, nil, nil)
	if err != nil {
		return profile, err
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		return profile, err
	}
	if profile.Execution != ref || profile.Profile == "" || profile.Capabilities == nil ||
		len(profile.Observed) == 0 || profile.Observed[0] != '{' {
		return profile, errors.New("Worker returned an incomplete resource observation for this execution")
	}
	return profile, nil
}

// Resize reuses the same Worker operation and execution after a lost response.
// SubmitNativeResize returns the existing durable receipt for an identical request.
func (c Client) Resize(ctx context.Context, id string, ref Execution, change ResourceChange) (ResizeReceipt, error) {
	var reply struct {
		ResizeReceipt
		ID          string `json:"operation_id"`
		Digest      string `json:"request_digest"`
		Kind        string `json:"kind"`
		Route       string `json:"route_id"`
		Bundle      string `json:"bundle_digest"`
		Instance    string `json:"instance_id"`
		Incarnation string `json:"expected_incarnation_id"`
		Epoch       int64  `json:"expected_execution_epoch"`
	}
	body, err := json.Marshal([]any{"SubmitNativeResize", id, ref, change})
	if err != nil {
		return reply.ResizeReceipt, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	raw, _, err := c.call(ctx, request{Method: "SubmitNativeResize", OperationID: id, RequestDigest: digest, Expected: &ref, Payload: change}, nil, nil)
	if err != nil {
		return reply.ResizeReceipt, err
	}
	if err = json.Unmarshal(raw, &reply); err != nil {
		return reply.ResizeReceipt, workerReplyError("SubmitNativeResize", err)
	}
	if reply.ID != id || reply.Digest != digest || reply.Route != "deep" || reply.Kind != "resize_resources" ||
		reply.Bundle != c.Bundle || reply.Instance != ref.InstanceID || reply.Incarnation != ref.IncarnationID || reply.Epoch != ref.Epoch {
		return reply.ResizeReceipt, workerReplyError("SubmitNativeResize", errors.New("Worker resource operation receipt does not match the dispatched change"))
	}
	if reply.Resize != nil {
		seen := reply.Resize.Change
		if reply.Resize.OperationID != id || seen.Kind != change.Kind || seen.CPUs != change.CPUs || seen.MemoryMiB != change.MemoryMiB ||
			(seen.TargetMiB == nil) != (change.TargetMiB == nil) || seen.TargetMiB != nil && *seen.TargetMiB != *change.TargetMiB {
			return reply.ResizeReceipt, workerReplyError("SubmitNativeResize", errors.New("Worker returned progress for a different resource change"))
		}
	}
	return reply.ResizeReceipt, nil
}
