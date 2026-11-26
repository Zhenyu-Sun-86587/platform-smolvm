package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

// CancelNativeForkTx fences a queued dispatch or persists a request for the Worker
// that owns the cut. A concurrent observer cannot overwrite it with an older revision.
func (e *Engine) CancelNativeForkTx(ctx context.Context, tx *sql.Tx, id string, version int64) (registry.Operation, error) {
	op, err := e.DB.GetOperationTx(ctx, tx, id)
	if err != nil {
		return registry.Operation{}, err
	}
	if version != 0 && version != op.Version {
		return registry.Operation{}, registry.ErrRevision
	}
	if !rt.IsNativeFork(op.Kind) || op.Terminal() || op.State == registry.RecoveryRequired {
		return registry.Operation{}, registry.ErrOperationState
	}
	step := registry.TransitionRequest{ExpectedVersion: op.Version, State: registry.Cancelled, Effect: registry.EffectNone, Stage: "completed"}
	if op.State == registry.Queued {
		return e.DB.TransitionTx(ctx, tx, id, step)
	}
	var intent Intent
	if err := json.Unmarshal(op.Dispatch, &intent); err != nil {
		return registry.Operation{}, err
	}
	if intent.Fork == nil {
		if op.State == registry.Dispatching && op.Stage == "preflight" && intent.Stage == "preflight" {
			return e.DB.TransitionTx(ctx, tx, id, step)
		}
		return registry.Operation{}, registry.ErrOperationState
	}
	if intent.Fork.RetryRequested {
		return registry.Operation{}, registry.ErrOperationState
	}
	if intent.Fork.CancelRequested {
		return op, nil
	}
	intent.Fork.CancelRequested = true
	return e.DB.TransitionTx(ctx, tx, id, registry.TransitionRequest{
		ExpectedVersion: op.Version, State: op.State, Stage: "fork_cancel_requested", Dispatch: JSON(intent),
	})
}

func (j *Job) reconcileForkCancellation(ctx context.Context, control rt.NativeForkControl, current rt.OperationReceipt) error {
	intent := j.Intent.Fork
	if err := j.checkForkReceipt(current); err != nil {
		return j.fail(ctx, err)
	}
	if current.Phase != "NEW" {
		intent.CancelRequested = false
		return j.confirmNativeFork(ctx, control, current)
	}
	if err := j.checkForkAttempts(current); err != nil {
		return j.fail(ctx, err)
	}
	cancel, ok := control.(rt.ForkCancelControl)
	if ok {
		request := intent.Submission
		updated, err := cancel.CancelFork(ctx, j.Op.ID, rt.ForkControl{
			RequestDigest: request.RequestDigest, Expected: request.Expected, ExpectedRevision: current.Revision,
		})
		if err == nil {
			if updated.Fork == nil || updated.Fork.CaptureID != current.Fork.CaptureID || updated.Revision < current.Revision {
				return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker cancellation returned a different cut or an older revision"})
			}
			intent.CancelRequested = false
			return j.confirmNativeFork(ctx, control, updated)
		}
		if errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain) {
			return err
		}
		var fault *rt.Error
		if errors.As(err, &fault) && (fault.NativeCode == "E_STALE_REVISION" || fault.NativeCode == "E_ALREADY_COMMITTED") {
			// Re-read the same cut. A child may have passed its commit boundary.
			return nil
		}
		if !errors.Is(err, rt.ErrUnsupported) && !launchReceiptMissing(err) {
			return j.fail(ctx, err)
		}
	}
	intent.CancelRequested = false
	return j.recordForkProgress(ctx, registry.TransitionRequest{
		State: registry.Observing, Stage: current.Fork.Stage, Dispatch: JSON(j.Intent),
		Error: JSON(map[string]string{"code": "CAPABILITY_UNSUPPORTED", "message": "pinned Worker cannot cancel an active fork"}),
	})
}
