package smolvm

import (
	"context"
	"errors"
	"net/http"
	"time"

	"sandbox-platform/internal/runtime"
)

const (
	executionHandleChanged = "machine execution changed since this handle was acquired"
	noRunningHandle        = "machine is not running and autoStart is false"
)

// A successful detached Resume or Live Fork can publish the execution before
// serve's supervisor updates its in-memory manager. v1.37.1 checks discovery
// every 5 seconds. Only the two refusals raised before the requested operation
// have a retry path. A lost response always returns to the operation owner.
func retryAttachment[T any](ctx context.Context, client *Client, name, refusal string, first T, err error, run func(context.Context) (T, error)) (T, error) {
	if !attachConflict(err, refusal) {
		return first, err
	}
	original, observeErr := client.Observe(ctx, name)
	if observeErr != nil || original.State != runtime.StateRunning || !original.Identity.Complete() {
		return first, err
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return first, err
		case <-ctx.Done():
			return first, err
		case <-tick.C:
			observed, observeErr := client.Observe(ctx, name)
			if observeErr != nil || observed.State != runtime.StateRunning || !observed.Identity.Equal(original.Identity) {
				return first, err
			}
			result, retryErr := run(ctx)
			if !attachConflict(retryErr, refusal) {
				return result, retryErr
			}
		}
	}
}

func attachConflict(err error, refusal string) bool {
	var native *runtime.Error
	return errors.As(err, &native) && !native.Unknown && native.Status == http.StatusConflict &&
		native.NativeCode == "CONFLICT" && native.Message == refusal
}
