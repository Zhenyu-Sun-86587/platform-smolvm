package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	rt "sandbox-platform/internal/runtime"
)

// LifecycleControl is the binding-scoped lifecycle and Guest Agent boundary.
type LifecycleControl interface {
	Lifecycle(context.Context, string, string, Execution, any, any) error
	OpenScopedGuest(context.Context, Execution) (net.Conn, error)
}

func (c Client) OpenScopedGuest(ctx context.Context, execution Execution) (net.Conn, error) {
	return c.OpenGuestAgent(ctx, execution)
}

func lifecycleMethod(name string) bool {
	switch name {
	case "QueryLifecycleEvents", "QueryHostActivity", "GetLifecycleStatus", "ApplyLifecyclePolicy", "ReportActivity", "AcquireActivityHold", "ReleaseActivityHold",
		"AcquireRunningLease", "RenewRunningLease", "ReleaseRunningLease", "RequestWake", "AuthorizeWakeActivation", "CancelWakeDemand", "CloseAccessProtection", "QueryProtection", "RetireStoppedInstance":
		return true
	}
	return false
}
func (c Client) Lifecycle(ctx context.Context, method, key string, expected Execution, payload, into any) error {
	if !lifecycleMethod(method) {
		return fmt.Errorf("unsupported Worker lifecycle method %q", method)
	}
	q := request{Method: method, Expected: &expected, Payload: payload}
	if key != "" {
		digest, err := LifecycleDigest(method, key, expected, payload)
		if err != nil {
			return err
		}
		q.OperationID = key
		q.RequestDigest = digest
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

func LifecycleDigest(method, key string, expected Execution, payload any) (string, error) {
	data, err := json.Marshal([]any{method, key, expected, payload})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

// RetryDelivery reports failures for which the same durable request may still
// have a receipt. A rejected revision or execution requires a new caller decision.
func RetryDelivery(err error) bool {
	var fault *Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "E_CATALOG", "E_RECOVERING", "E_HOST_IO", "E_BUSY", "E_CPU_QUOTA_UNCERTAIN", "E_CPU_QUOTA_UNAVAILABLE":
			return true
		}
		return false
	}
	if errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain) {
		return true
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
