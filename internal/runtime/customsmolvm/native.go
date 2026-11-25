package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

var _ rt.NativeControl = (*Adapter)(nil)

func validExecution(ref rt.ExecutionRef) bool {
	return ref.ExecutionEpoch > 0 && identifier.MatchString(ref.InstanceID) && identifier.MatchString(ref.IncarnationID)
}

func (a *Adapter) DiscoverExecution(ctx context.Context, name string) (rt.ExecutionRef, error) {
	if err := smolvm.ValidateName(name); err != nil {
		return rt.ExecutionRef{}, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return rt.ExecutionRef{}, err
	}
	defer endpoint.CloseIdleConnections()
	var ref rt.ExecutionRef
	if err := get(ctx, endpoint, extensionPath+"/machines/"+name+"/execution", &ref); err != nil {
		return rt.ExecutionRef{}, err
	}
	if !validExecution(ref) {
		return rt.ExecutionRef{}, fmt.Errorf("%w: incomplete registered execution", rt.ErrUnsupported)
	}
	return ref, nil
}

func (a *Adapter) NativeInventory(ctx context.Context, expected rt.ExecutionRef) (rt.NativeInventory, error) {
	if !validExecution(expected) {
		return rt.NativeInventory{}, fmt.Errorf("%w: invalid registered execution", rt.ErrInvalid)
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return rt.NativeInventory{}, err
	}
	defer endpoint.CloseIdleConnections()
	var result rt.NativeInventory
	if err := get(ctx, endpoint, extensionPath+"/instances/"+expected.InstanceID+"/inventory", &result); err != nil {
		return rt.NativeInventory{}, err
	}
	if result.Execution.ExecutionRef != expected || result.Execution.BundleDigest != a.route.Bundle ||
		result.Execution.RouteID != "deep" || result.Execution.Phase != "registered" ||
		result.Inventory.Protocol != rt.ManagedStandbyContract || result.Inventory.Devices == nil {
		return rt.NativeInventory{}, fmt.Errorf("%w: incompatible native inventory", rt.ErrUnsupported)
	}
	result.Capabilities = handshake.Capabilities
	return result, nil
}

func (a *Adapter) SubmitNative(ctx context.Context, request rt.NativeSubmission) (OperationReceipt, error) {
	if !validExecution(request.Expected) || !identifier.MatchString(request.OperationID) ||
		!validDigest(request.RequestDigest) || rt.NativePowerCapability(request.Kind) == "" {
		return OperationReceipt{}, fmt.Errorf("%w: invalid native operation", rt.ErrInvalid)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return OperationReceipt{}, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer endpoint.CloseIdleConnections()
	var receipt OperationReceipt
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/operations", payload, &receipt); err != nil {
		return OperationReceipt{}, err
	}
	if err := validateReceipt(receipt, a.route.Bundle, request.OperationID); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
