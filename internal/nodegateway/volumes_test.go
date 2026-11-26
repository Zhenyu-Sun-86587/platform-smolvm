package nodegateway

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

type fakeVolumes struct {
	*fakeRuntime
	bundle  string
	applied int
}

func (f *fakeVolumes) DiscoverVolumeConsumer(context.Context, string) (rt.ExecutionRef, error) {
	return rt.ExecutionRef{InstanceID: testRef.InstanceID}, nil
}

func (f *fakeVolumes) ObserveVolume(_ context.Context, volume, owner string) (rt.VolumeObservation, error) {
	return rt.VolumeObservation{Schema: "Volume/1", ID: volume, OwnerID: owner, BundleDigest: f.bundle, RouteID: "deep",
		HeadResourceID: f.name, Revision: 1, State: "available", CapacityBytes: 512}, nil
}

func (f *fakeVolumes) ApplyVolume(ctx context.Context, in rt.VolumeMutation) (rt.VolumeReceipt, error) {
	f.applied++
	if in.OwnerID == "wrong-owner" {
		return rt.VolumeReceipt{}, &rt.Error{Code: "NOT_FOUND", NativeCode: "E_NOT_FOUND"}
	}
	v, err := f.ObserveVolume(ctx, in.VolumeID, in.OwnerID)
	return rt.VolumeReceipt{Schema: "VolumeOperation/1", OperationID: in.OperationID, Volume: v}, err
}

func (f *fakeVolumes) Admit(kind string) error {
	if kind == "configure" || kind == "service_create" {
		return nil
	}
	return rt.ErrUnsupported
}

func TestVolumeScopeReplayAndAdmittedActions(t *testing.T) {
	ctx := context.Background()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	volA := &fakeVolumes{fakeRuntime: fake("worker-a-head"), bundle: testDigest}
	volB := &fakeVolumes{fakeRuntime: fake("worker-b-head"), bundle: testDigest}
	j := testJournal(t, filepath.Join(t.TempDir(), "node"), testAuthority)
	a := testNode(t, ca, testAuthority, testRoute, volA, j, controller, pin, nil)
	authorityB, routeB := testAuthority, testRoute
	authorityB.WorkerID, routeB.RuntimeInstanceID = "worker-b", "runtime-b"
	b := testNode(t, ca, authorityB, routeB, volB,
		testJournal(t, filepath.Join(t.TempDir(), "node"), authorityB), controller, pin, nil)
	aAdapter, bAdapter := a.client.Adapter(testRoute), b.client.Adapter(routeB)
	if err := aAdapter.Admit("configure"); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("missing handshake should not advertise write capabilities: %v", err)
	}
	if _, err := a.client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if err := aAdapter.Admit("configure"); err != nil {
		t.Fatal(err)
	}
	if err := aAdapter.Admit("arbitrary_method"); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("untyped action was admitted: %v", err)
	}

	volume := rt.VolumeMutation{OperationID: "volume-create", RequestDigest: testDigest,
		VolumeID: "same-volume-id", OwnerID: "owner-a",
		Command: json.RawMessage(`{"action":"create","lifecycle":"persistent","capacity_bytes":512}`)}
	first, err := aAdapter.ApplyVolume(ctx, volume)
	if err != nil || first.Volume.HeadResourceID != "worker-a-head" {
		t.Fatalf("unexpected Worker volume: %+v / %v", first, err)
	}
	again, err := aAdapter.ApplyVolume(ctx, volume)
	if err != nil || again.OperationID != first.OperationID || volA.applied != 1 {
		t.Fatalf("same ID replay did not use original receipt: %+v / %v / %d", again, err, volA.applied)
	}
	changed := volume
	changed.Command = json.RawMessage(`{"action":"create","lifecycle":"persistent","capacity_bytes":1024}`)
	if _, err := aAdapter.ApplyVolume(ctx, changed); !errors.Is(err, rt.ErrConflict) {
		t.Fatalf("changed volume request with original ID/digest accepted: %v", err)
	}
	if _, err := aAdapter.SubmitNative(ctx, request(volume.OperationID)); !errors.Is(err, rt.ErrConflict) {
		t.Fatalf("volume operation ID was accepted by native power endpoint: %v", err)
	}
	observed, err := bAdapter.ObserveVolume(ctx, volume.VolumeID, volume.OwnerID)
	if err != nil || observed.HeadResourceID != "worker-b-head" || volB.applied != 0 {
		t.Fatalf("matching local volume IDs leaked Worker routing: %+v / %v", observed, err)
	}
	wrong := testRoute
	wrong.BundleDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := a.client.Adapter(wrong).ObserveVolume(ctx, volume.VolumeID, volume.OwnerID); nodeproto.FailureCode(err) != nodeproto.CodeRoute {
		t.Fatalf("different Bundle accepted: %v", err)
	}
	invalid := volume
	invalid.Command = json.RawMessage(`{"action":"create","lifecycle":"persistent","capacity_bytes":512,"path":"/root/unsafe"}`)
	if _, err := aAdapter.ApplyVolume(ctx, invalid); !errors.Is(err, rt.ErrInvalid) {
		t.Fatalf("volume allowed untyped path field: %v", err)
	}

	denied := volume
	denied.OperationID, denied.OwnerID = "volume-with-wrong-owner", "wrong-owner"
	for range 2 {
		if _, err := aAdapter.ApplyVolume(ctx, denied); !errors.Is(err, rt.ErrNotFound) {
			t.Fatalf("definitive native owner refusal failed: %v", err)
		}
	}
	if volA.applied != 2 {
		t.Fatalf("same explicit refusal re-executed: %d", volA.applied)
	}

	missing := nodeproto.VolumeMutation{OperationID: "volume-crash", RequestDigest: testDigest, VolumeID: volume.VolumeID,
		OwnerID: volume.OwnerID, Command: nodeproto.VolumeCommand{Action: "create", Lifecycle: "persistent", CapacityBytes: 512}}
	raw, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, send, err := j.beginVolume(ctx, testRoute, missing, fmt.Sprintf("sha256:%x", sha256.Sum256(raw))); err != nil || !send {
		t.Fatalf("could not set volume crash fence: %v / %v", send, err)
	}
	volume.OperationID = missing.OperationID
	if _, err := aAdapter.ApplyVolume(ctx, volume); !errors.Is(err, rt.ErrUncertain) || volA.applied != 2 {
		t.Fatalf("crash-window volume request was replayed: %d / %v", volA.applied, err)
	}
	out, err := aAdapter.ReconcileKnown(ctx, nodeproto.ReconcileRequest{})
	if err != nil || len(out.Operations) != 1 || out.Operations[0].OperationID != missing.OperationID ||
		out.Operations[0].Issue != nodeproto.CodeUnknown {
		t.Fatalf("unresolved volume overlooked in reconciliation: %+v / %v", out, err)
	}
}
