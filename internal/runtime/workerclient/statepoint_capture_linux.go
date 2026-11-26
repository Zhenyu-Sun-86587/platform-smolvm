package workerclient

import (
	"context"
	"encoding/json"
	"errors"
)

// StatePointCapture reads the durable record for a completed capture operation.
// Callers project only the fields needed from the private Worker response.
func (c Client) StatePointCapture(ctx context.Context, ref Execution, operation, digest, capture string) (json.RawMessage, error) {
	if operation == "" || digest == "" || capture == "" {
		return nil, errors.New("StatePoint observation requires a capture operation")
	}
	payload := struct {
		CaptureID string `json:"capture_id"`
	}{capture}
	result, _, err := c.call(ctx, request{Method: "QueryStatePoint", Expected: &ref, OperationID: operation,
		RequestDigest: digest, Payload: payload}, nil, nil)
	return result, err
}
