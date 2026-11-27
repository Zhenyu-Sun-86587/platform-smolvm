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
	profiles, err := a.SystemLaunchCapabilities(ctx)
	return profiles.Boot, err
}

func (a *Adapter) SystemLaunchCapabilities(ctx context.Context) (rt.SystemLaunchProfiles, error) {
	unknown := rt.FullStateCapabilities(false)[rt.LiveComputeGrowthCapability]
	system, err := a.manifestSystemBoot()
	if err != nil {
		return rt.SystemLaunchProfiles{Boot: unknown, Growth: unknown, KernelCow: unknown}, err
	}
	if !system {
		missing := rt.Capability{Status: "unsupported", ReasonCode: "BUNDLE_BOOT_PROFILE_UNAVAILABLE"}
		return rt.SystemLaunchProfiles{Boot: missing, Growth: missing, KernelCow: missing}, nil
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return rt.SystemLaunchProfiles{Boot: unknown, Growth: unknown, KernelCow: unknown}, err
	}
	defer endpoint.CloseIdleConnections()
	boot := systemBootCapability(handshake)
	growth := handshake.Capabilities[rt.LiveComputeGrowthCapability]
	if !boot.Supported || growth.Status != "supported" || !growth.Supported || growth.ContractVersion != 1 || growth.VerifiedProfile != rt.LiveComputeGrowthProfile {
		growth = unknown
	}
	return rt.SystemLaunchProfiles{Boot: boot, Growth: growth, KernelCow: kernelCowLaunch(boot, handshake.Capabilities[rt.KernelCowLaunchCapability])}, nil
}

func kernelCowLaunch(boot, cow rt.Capability) rt.Capability {
	if boot.Supported && cow.Status == "supported" && cow.Supported && cow.ContractVersion == 1 && cow.VerifiedProfile == rt.KernelCowProfile {
		return cow
	}
	if cow.Status == "unsupported" && !cow.Supported && cow.ReasonCode != "" {
		return cow
	}
	return rt.Capability{Status: "unknown", ReasonCode: "KERNEL_COW_PROFILE_UNVERIFIED"}
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
	if profile := request.Parameters.MemoryForkProfile; profile != "" {
		c := kernelCowLaunch(systemBootCapability(handshake), handshake.Capabilities[rt.KernelCowLaunchCapability])
		if profile != rt.KernelCowProfile || request.Parameters.LiveComputeGrowth {
			return OperationReceipt{}, rt.ErrInvalid
		}
		if !c.Supported {
			return OperationReceipt{}, &rt.CapabilityError{Capability: rt.KernelCowLaunchCapability, Status: c}
		}
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
