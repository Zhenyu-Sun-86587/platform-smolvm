package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

func lifecycleMethod(name string) bool {
	switch name {
	case "QueryLifecycleEvents", "QueryHostActivity", "GetLifecycleStatus", "ApplyLifecyclePolicy", "ReportActivity", "AcquireActivityHold", "ReleaseActivityHold",
		"AcquireRunningLease", "RenewRunningLease", "ReleaseRunningLease", "RequestWake", "AuthorizeWakeActivation", "CancelWakeDemand", "QueryProtection":
		return true
	}
	return false
}
func (c Client) Lifecycle(ctx context.Context, method, key string, expected Execution, payload, into any) error {
	if !lifecycleMethod(method) {
		return fmt.Errorf("unsupported Worker lifecycle method %q", method)
	}
	q := request{Method: method, Expected: expected, Payload: payload}
	if key != "" {
		data, e := json.Marshal([]any{method, key, expected, payload})
		if e != nil {
			return e
		}
		q.OperationID = key
		q.RequestDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	}
	raw, _, err := c.call(ctx, q, nil, nil)
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}
