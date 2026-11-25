package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// RetryDelivery reports failures for which the same durable request may still
// have a receipt. A rejected revision or execution requires a new caller decision.
func RetryDelivery(err error) bool {
	var fault *Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "E_CATALOG", "E_RECOVERING", "E_HOST_IO", "E_BUSY":
			return true
		}
		return false
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
