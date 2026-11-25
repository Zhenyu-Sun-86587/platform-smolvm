package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (e *Engine) RetryNativeFork(ctx context.Context, tx *sql.Tx, op registry.Operation, version int64) (registry.Operation, error) {
	var intent Intent
	if !rt.IsNativeFork(op.Kind) || json.Unmarshal(op.Dispatch, &intent) != nil || intent.Fork == nil || intent.Fork.RetryRequested {
		return registry.Operation{}, registry.ErrOperationState
	}
	intent.Fork.RetryRequested = true
	intent.Stage = "fork_retry"
	return e.DB.BeginNativeForkRetryTx(ctx, tx, op.ID, version, JSON(intent))
}

func (j *Job) reconcileForkRetry(ctx context.Context, query rt.NativeForkControl, receipt rt.OperationReceipt) error {
	if err := j.checkForkReceipt(receipt); err != nil {
		return j.fail(ctx, err)
	}
	intent := j.Intent.Fork
	if intent.RetryFrom == nil {
		if !j.settledRetrySource(receipt) || (intent.Receipt != nil && !sameSettledFork(receipt, *intent.Receipt)) {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork differs from the previously verified terminal result"})
		}
		intent.RetryFrom = &receipt
		if err := j.Stage(ctx, "worker_fork_retry", ""); err != nil {
			return err
		}
	}
	previous := *intent.RetryFrom
	if receipt.Revision < previous.Revision {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork retry revision regressed"})
	}
	if receipt.Revision > previous.Revision {
		return j.confirmForkRetry(ctx, receipt)
	}
	if !sameSettledFork(receipt, previous) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker fork changed without a new revision"})
	}
	control, ok := j.Runtime.(rt.ForkRetryControl)
	if !ok {
		return j.rejectForkRetry(ctx, &Failure{"CAPABILITY_UNSUPPORTED", "pinned Worker cannot retry fork targets"})
	}
	submission := intent.Submission
	updated, err := control.RetryFork(ctx, j.Op.ID, rt.ForkControl{
		RequestDigest: submission.RequestDigest, Expected: submission.Expected, ExpectedRevision: previous.Revision,
	})
	if err == nil {
		return j.confirmForkRetry(ctx, updated)
	}
	if errors.Is(err, rt.ErrUnavailable) || errors.Is(err, rt.ErrUncertain) {
		return err
	}
	refused := statePointRefusal(err)
	var fault *rt.Error
	if errors.As(err, &fault) && fault.NativeCode == "E_STALE_REVISION" {
		refused = &Failure{"REVISION_CONFLICT", "Worker retry revision changed"}
	}
	if refused == nil && !launchReceiptMissing(err) && !errors.Is(err, rt.ErrUnsupported) {
		return j.fail(ctx, err)
	}
	observed, observeErr := query.QueryOperation(ctx, j.Op.ID)
	if observeErr != nil {
		return observeErr
	}
	if observed.Revision > previous.Revision {
		return j.confirmForkRetry(ctx, observed)
	}
	if err := j.checkForkReceipt(observed); err != nil {
		return j.fail(ctx, err)
	}
	if !sameSettledFork(observed, previous) {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker retry refused with an inconsistent fork result"})
	}
	if refused == nil {
		refused = &Failure{"CAPABILITY_UNSUPPORTED", "pinned Worker cannot retry fork targets"}
	}
	return j.rejectForkRetry(ctx, refused)
}

func (j *Job) settledRetrySource(receipt rt.OperationReceipt) bool {
	if receipt.Revision < 1 || receipt.Phase != "COMPLETED" || receipt.Decision != "NONE" ||
		receipt.Fork.Stage != "complete" || !receipt.Fork.SourceReleased {
		return false
	}
	for ordinal, child := range j.Op.Children {
		observed := receipt.Fork.Children[ordinal]
		if child.Attempt != observed.Attempt || (child.State == "ready" && observed.Phase != "COMPLETED") ||
			(child.State == "quarantined" && observed.Phase != "ABORTED") || (child.State != "ready" && child.State != "quarantined") ||
			(child.ErrorCode == "NATIVE_CHILD_ABORTED" && observed.Cancelled) || (child.Attempt == 0 && observed.ExecutionEpoch != 1) {
			return false
		}
	}
	return true
}

func sameSettledFork(current, previous rt.OperationReceipt) bool {
	if current.OperationID != previous.OperationID || current.RequestDigest != previous.RequestDigest ||
		current.Fork == nil || previous.Fork == nil || current.Phase != previous.Phase || current.Decision != previous.Decision ||
		current.Revision != previous.Revision || !current.Fork.SourceReleased || current.Fork.Stage != previous.Fork.Stage ||
		current.Fork.CaptureID != previous.Fork.CaptureID || len(current.Fork.Children) != len(previous.Fork.Children) {
		return false
	}
	for i, child := range current.Fork.Children {
		if !sameSettledForkChild(child, previous.Fork.Children[i]) {
			return false
		}
	}
	return true
}

func sameSettledForkChild(a, b rt.ForkChildReceipt) bool {
	return sameForkAttempt(a, b) && sameForkAttempt(b, a) && a.Phase == b.Phase
}

func (j *Job) confirmForkRetry(ctx context.Context, receipt rt.OperationReceipt) error {
	if err := j.checkForkReceipt(receipt); err != nil {
		return j.fail(ctx, err)
	}
	intent := j.Intent.Fork
	previous := intent.RetryFrom
	if receipt.Revision <= previous.Revision || receipt.Decision != "NONE" ||
		(receipt.Phase != "NEW" && receipt.Phase != "COMPLETED") {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker did not acknowledge the fork retry"})
	}
	step := registry.TransitionRequest{State: registry.Observing, Stage: receipt.Fork.Stage, Effect: j.Op.Effect,
		Result: j.Op.Result, SourceDisposition: j.Op.SourceDisposition}
	for ordinal, child := range receipt.Fork.Children {
		old := previous.Fork.Children[ordinal]
		if sameSettledForkChild(child, old) {
			continue
		}
		if j.Op.Children[ordinal].ErrorCode != "NATIVE_CHILD_ABORTED" || old.Phase != "ABORTED" || old.Cancelled ||
			child.InstanceID != old.InstanceID || child.OperationID == old.OperationID || child.Attempt != old.Attempt+1 ||
			child.ExecutionEpoch < old.ExecutionEpoch || child.ExecutionEpoch > old.ExecutionEpoch+1 ||
			(child.IncarnationID != nil && old.IncarnationID != nil && *child.IncarnationID == *old.IncarnationID) {
			return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker retry changed a previously committed child or its logical identity"})
		}
		step.ForkRetryOrdinals = append(step.ForkRetryOrdinals, ordinal)
	}
	if len(step.ForkRetryOrdinals) == 0 {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker retry has no new child attempt"})
	}
	intent.Receipt, intent.RetryRequested, intent.RetryFrom = &receipt, false, nil
	step.Dispatch = JSON(j.Intent)
	if j.Op.Effect == registry.EffectNone {
		step.Effect = registry.EffectUnknown
	}
	return j.recordForkProgress(ctx, step)
}

func (j *Job) rejectForkRetry(ctx context.Context, reason error) error {
	intent := j.Intent.Fork
	intent.Receipt, intent.RetryRequested, intent.RetryFrom = intent.RetryFrom, false, nil
	step := registry.TransitionRequest{Effect: j.Op.Effect, Result: j.Op.Result, Dispatch: JSON(j.Intent),
		Error: JSON(map[string]string{"code": "NATIVE_FORK_RETRY_REFUSED", "message": reason.Error()})}
	state := registry.Failed
	if j.Op.Effect == registry.EffectPartial {
		state = registry.Succeeded
	}
	return j.finish(ctx, state, step)
}
