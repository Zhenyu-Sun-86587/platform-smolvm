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
