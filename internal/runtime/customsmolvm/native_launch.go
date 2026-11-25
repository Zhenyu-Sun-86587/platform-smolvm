package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

var _ rt.NativeLauncher = (*Adapter)(nil)

func (a *Adapter) manifestSystemBoot() (bool, error) {
	manifest, err := a.route.LoadManifest()
	if err != nil {
		return false, fmt.Errorf("%w: cannot verify the installed Bundle manifest: %v", rt.ErrUnsupported, err)
	}
	var profile struct {
		Guest struct {
			BootProfiles []string `json:"boot_profiles"`
		} `json:"guest"`
	}
	if err := json.Unmarshal(manifest, &profile); err != nil {
		return false, fmt.Errorf("%w: invalid Bundle boot profiles", rt.ErrUnsupported)
	}
	return slices.Contains(profile.Guest.BootProfiles, rt.SystemBootProfile), nil
}

func systemBootCapability(handshake Handshake) rt.Capability {
	capability := handshake.Capabilities[rt.SystemBootCapability]
	if capability.Status == "unsupported" {
		return capability
	}
	if capability.Status == "supported" && capability.Supported && capability.VerifiedProfile == rt.SystemBootProfile && capability.ContractVersion == 1 {
		return capability
	}
	return rt.FullStateCapabilities(false)[rt.SystemBootCapability]
}

func (a *Adapter) SystemBoot(ctx context.Context) (rt.Capability, error) {
	system, err := a.manifestSystemBoot()
	if err != nil {
		return rt.Capability{}, err
	}
	if !system {
		return rt.Capability{Status: "unsupported", ReasonCode: "BUNDLE_BOOT_PROFILE_UNAVAILABLE"}, nil
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return rt.Capability{}, err
	}
	defer endpoint.CloseIdleConnections()
	return systemBootCapability(handshake), nil
}

func (a *Adapter) DiscoverLaunchBinding(ctx context.Context, name string) (rt.LaunchBinding, error) {
	if err := smolvm.ValidateName(name); err != nil {
		return rt.LaunchBinding{}, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return rt.LaunchBinding{}, err
	}
	defer endpoint.CloseIdleConnections()
	var binding rt.LaunchBinding
	if err := get(ctx, endpoint, extensionPath+"/machines/"+name+"/binding", &binding); err != nil {
		return rt.LaunchBinding{}, err
	}
	if !identifier.MatchString(binding.InstanceID) || binding.ExecutionEpoch < 0 ||
		(binding.IncarnationID != nil && (binding.ExecutionEpoch == 0 || !identifier.MatchString(*binding.IncarnationID))) {
		return rt.LaunchBinding{}, fmt.Errorf("%w: invalid native launch binding", rt.ErrUnsupported)
	}
	return binding, nil
}

func (a *Adapter) SubmitLaunch(ctx context.Context, request rt.LaunchSubmission) (OperationReceipt, error) {
	if err := smolvm.ValidateName(request.Parameters.Name); err != nil {
		return OperationReceipt{}, err
	}
	if !identifier.MatchString(request.Expected.InstanceID) || request.Expected.ExecutionEpoch < 0 || request.Expected.IncarnationID != nil ||
		!identifier.MatchString(request.OperationID) || !validDigest(request.RequestDigest) || request.Kind != rt.Launch ||
		request.Parameters.BootProfile != rt.SystemBootProfile {
		return OperationReceipt{}, fmt.Errorf("%w: invalid System launch request", rt.ErrInvalid)
	}
	system, err := a.manifestSystemBoot()
	if err != nil {
		return OperationReceipt{}, err
	}
	if !system {
		return OperationReceipt{}, rt.SystemBootUnavailable(false)
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
	if capability := systemBootCapability(handshake); !capability.Supported {
		return OperationReceipt{}, &rt.CapabilityError{Capability: rt.SystemBootCapability, Status: capability}
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
