package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

var _ rt.NativeForkControl = (*Adapter)(nil)
var _ rt.ForkRetryControl = (*Adapter)(nil)
var _ rt.ForkCancelControl = (*Adapter)(nil)

func (a *Adapter) RetryFork(ctx context.Context, id string, control rt.ForkControl) (OperationReceipt, error) {
	return a.controlFork(ctx, id, "retry", control)
}

func (a *Adapter) CancelFork(ctx context.Context, id string, control rt.ForkControl) (OperationReceipt, error) {
	return a.controlFork(ctx, id, "cancel", control)
}

func (a *Adapter) controlFork(ctx context.Context, id, action string, control rt.ForkControl) (OperationReceipt, error) {
	if !identifier.MatchString(id) || !validDigest(control.RequestDigest) || !validExecution(control.Expected) || control.ExpectedRevision < 1 {
		return OperationReceipt{}, fmt.Errorf("%w: invalid fork %s scope", rt.ErrInvalid, action)
	}
	payload, err := json.Marshal(control)
	if err != nil {
		return OperationReceipt{}, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer endpoint.CloseIdleConnections()
	var receipt OperationReceipt
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/operations/"+id+"/"+action, payload, &receipt); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, validateReceipt(receipt, a.route.Bundle, id)
}

func (a *Adapter) SubmitFork(ctx context.Context, request rt.ForkSubmission) (OperationReceipt, error) {
	children := request.Parameters.Children
	if !validExecution(request.Expected) || !identifier.MatchString(request.OperationID) || !validDigest(request.RequestDigest) ||
		!rt.IsNativeFork(request.Kind) || len(children) < 1 || len(children) > rt.MaxForkChildren ||
		(request.Kind == rt.IndependentFork && len(children) != 1) {
		return OperationReceipt{}, fmt.Errorf("%w: invalid native fork request", rt.ErrInvalid)
	}
	for _, child := range children {
		if err := smolvm.ValidateName(child); err != nil {
			return OperationReceipt{}, err
		}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return OperationReceipt{}, err
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer endpoint.CloseIdleConnections()
	if !rt.VerifiedForkProfile(handshake.Capabilities[rt.FullStateOperationCapability(request.Kind)]) {
		return OperationReceipt{}, rt.FullStateUnavailable(request.Kind, false)
	}
	var receipt OperationReceipt
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/operations", payload, &receipt); err != nil {
		return OperationReceipt{}, err
	}
	if err := validateReceipt(receipt, a.route.Bundle, request.OperationID); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
