package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

var _ rt.VolumeControl = (*Adapter)(nil)

func (a *Adapter) DiscoverVolumeConsumer(ctx context.Context, name string) (rt.ExecutionRef, error) {
	var ref rt.ExecutionRef
	if err := smolvm.ValidateName(name); err != nil {
		return ref, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return ref, err
	}
	defer endpoint.CloseIdleConnections()
	if err := get(ctx, endpoint, extensionPath+"/machines/"+name+"/binding", &ref); err != nil {
		return ref, err
	}
	if !identifier.MatchString(ref.InstanceID) || ref.ExecutionEpoch < 0 {
		return ref, rt.ErrUnsupported
	}
	if ref.IncarnationID != "" {
		return ref, fmt.Errorf("%w: volume topology requires a fenced stopped execution", rt.ErrConflict)
	}
	return ref, nil
}

func (a *Adapter) ObserveVolume(ctx context.Context, volume, owner string) (rt.VolumeObservation, error) {
	var result rt.VolumeObservation
	if !identifier.MatchString(volume) || !identifier.MatchString(owner) {
		return result, rt.ErrInvalid
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return result, err
	}
	defer endpoint.CloseIdleConnections()
	if err := get(ctx, endpoint, extensionPath+"/volumes/"+volume+"?owner_id="+url.QueryEscape(owner), &result); err != nil {
		return result, err
	}
	return result, a.verifyVolume(result, volume, owner)
}

func (a *Adapter) ApplyVolume(ctx context.Context, input rt.VolumeMutation) (rt.VolumeReceipt, error) {
	var result rt.VolumeReceipt
	if !identifier.MatchString(input.VolumeID) || !identifier.MatchString(input.OwnerID) ||
		!identifier.MatchString(input.OperationID) || !validDigest(input.RequestDigest) || input.ExpectedRevision < 0 {
		return result, rt.ErrInvalid
	}
	data, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return result, err
	}
	defer endpoint.CloseIdleConnections()
	endpoint.Timeout = 125 * time.Second
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/volumes", data, &result); err != nil {
		return result, err
	}
	if result.Schema != "VolumeOperation/1" || result.OperationID != input.OperationID {
		return result, fmt.Errorf("%w: incompatible volume operation receipt", rt.ErrUncertain)
	}
	if err := a.verifyVolume(result.Volume, input.VolumeID, input.OwnerID); err != nil {
		return result, fmt.Errorf("%w: %v", rt.ErrUncertain, err)
	}
	return result, nil
}

func (a *Adapter) verifyVolume(result rt.VolumeObservation, volume, owner string) error {
	if result.Schema != "Volume/1" || result.ID != volume || result.OwnerID != owner ||
		result.RouteID != "deep" || result.BundleDigest != a.route.Bundle || result.Revision < 1 ||
		(result.State != "available" && result.State != "changing" && result.State != "deleted") ||
		(result.State == "available" && (result.CapacityBytes < 1 || !identifier.MatchString(result.HeadResourceID))) {
		return fmt.Errorf("%w: volume belongs to a different Worker context", rt.ErrUnsupported)
	}
	return nil
}
