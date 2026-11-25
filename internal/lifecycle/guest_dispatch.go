package lifecycle

import (
	"context"
	"errors"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (j *Job) checkGuestSubmission(ctx context.Context, query func(context.Context, string) (rt.OperationReceipt, error)) (*rt.OperationReceipt, error) {
	err := j.DB.CheckGuestDispatch(ctx, j.Op.ID)
	if !errors.Is(err, registry.ErrGuestAuthorityRevoked) {
		return nil, err
	}
	// A lost reply may belong to a submission authorized before revocation.
	// Reconcile the existing receipt without sending another mutation.
	prior, queryErr := query(ctx, j.Op.ID)
	if queryErr == nil {
		return &prior, nil
	}
	if !launchReceiptMissing(queryErr) {
		return nil, queryErr
	}
	j.Written = false
	return nil, j.fail(ctx, &Failure{"PERMISSION_DENIED", err.Error()})
}
