package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	rt "sandbox-platform/internal/runtime"
)

var _ rt.StatePointControl = (*Adapter)(nil)

func statePointCapability(handshake Handshake, kind string) rt.Capability {
	name := rt.FullStateOperationCapability(kind)
	capability := handshake.Capabilities[name]
	if capability.Status == "unsupported" || capability.Status == "unavailable" ||
		(capability.Status == "supported" && capability.Supported && capability.ContractVersion == 1 && capability.VerifiedProfile == "managed-block-v1") {
		return capability
	}
	return rt.FullStateCapabilities(false)[name]
}

func (a *Adapter) StatePointCapability(ctx context.Context, kind string) (rt.Capability, error) {
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return rt.Capability{}, err
	}
	defer endpoint.CloseIdleConnections()
	return statePointCapability(handshake, kind), nil
}

func (a *Adapter) SubmitStatePoint(ctx context.Context, request rt.StatePointSubmission) (rt.OperationReceipt, error) {
	var result rt.OperationReceipt
	if !rt.ManagedStatePoint(request.Kind) || !identifier.MatchString(request.OperationID) ||
		!validDigest(request.RequestDigest) || !identifier.MatchString(request.Parameters.CaptureID) ||
		!identifier.MatchString(request.Expected.InstanceID) || request.Expected.ExecutionEpoch < 0 ||
		(request.Kind == rt.VMRestore) != (request.Expected.IncarnationID == nil) {
		return result, rt.ErrInvalid
	}
	data, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return result, err
	}
	defer endpoint.CloseIdleConnections()
	if capability := statePointCapability(handshake, request.Kind); !capability.Supported {
		return result, &rt.CapabilityError{Capability: rt.FullStateOperationCapability(request.Kind), Status: capability}
	}
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/operations", data, &result); err != nil {
		return result, err
	}
	if err := validateReceipt(result, a.route.Bundle, request.OperationID); err != nil {
		return result, fmt.Errorf("%w: %v", rt.ErrUncertain, err)
	}
	return result, nil
}
