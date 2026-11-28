package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

var (
	_ rt.VolumeControl          = (*Adapter)(nil)
	_ rt.VolumeConsumerResolver = (*Adapter)(nil)
)

func (a *Adapter) DiscoverVolumeConsumer(ctx context.Context, name string) (rt.ExecutionRef, error) {
	return call[nodeproto.Name, rt.ExecutionRef](ctx, a, nodeproto.VolumeConsumerPath, nodeproto.Name{Name: name})
}

func (a *Adapter) ObserveVolume(ctx context.Context, volume, owner string) (rt.VolumeObservation, error) {
	in := nodeproto.VolumeLookup{VolumeID: volume, OwnerID: owner}
	out, err := call[nodeproto.VolumeLookup, rt.VolumeObservation](ctx, a, nodeproto.VolumeObservePath, in)
	if err == nil && !nodeproto.VolumeMatches(a.scope, in, out) {
		return rt.VolumeObservation{}, rt.ErrUnsupported
	}
	return out, err
}

func (a *Adapter) ApplyVolume(ctx context.Context, input rt.VolumeMutation) (rt.VolumeReceipt, error) {
	in := nodeproto.VolumeMutation{OperationID: input.OperationID, RequestDigest: input.RequestDigest, VolumeID: input.VolumeID,
		OwnerID: input.OwnerID, ExpectedRevision: input.ExpectedRevision}
	d := json.NewDecoder(bytes.NewReader(input.Command))
	d.DisallowUnknownFields()
	if err := d.Decode(&in.Command); err != nil {
		return rt.VolumeReceipt{}, rt.ErrInvalid
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return rt.VolumeReceipt{}, rt.ErrInvalid
	}
	out, err := call[nodeproto.VolumeMutation, rt.VolumeReceipt](ctx, a, nodeproto.VolumeApplyPath, in)
	if err == nil && (out.Schema != "VolumeOperation/1" || out.OperationID != input.OperationID ||
		!nodeproto.VolumeMatches(a.scope, nodeproto.VolumeLookup{VolumeID: input.VolumeID, OwnerID: input.OwnerID}, out.Volume)) {
		return rt.VolumeReceipt{}, &nodeproto.Failure{Code: nodeproto.CodeUnknown}
	}
	return out, err
}
