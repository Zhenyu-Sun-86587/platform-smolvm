package workerclient

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	rt "sandbox-platform/internal/runtime"
)

func TestWriteReplyLossPreservesUnknown(t *testing.T) {
	for _, method := range []string{"ApplyLifecyclePolicy", "SubmitNativeResize", "ApplyPrivatePacketWakeRule"} {
		err := workerReplyError(method, io.ErrUnexpectedEOF)
		if !errors.Is(err, rt.ErrUncertain) || !RetryDelivery(err) {
			t.Errorf("%s: lost response error = %v", method, err)
		}
	}
	for _, method := range []string{"GetLifecycleStatus", "QueryNativeResources", "QueryPrivatePacketWakeRule"} {
		if err := workerReplyError(method, io.ErrUnexpectedEOF); errors.Is(err, rt.ErrUncertain) {
			t.Errorf("%s: a lost read became an uncertain mutation: %v", method, err)
		}
	}
	rejected := &Fault{Code: "E_STALE_REVISION"}
	if err := workerReplyError("ApplyPrivatePacketWakeRule", rejected); err != rejected || RetryDelivery(err) {
		t.Fatalf("Worker's definitive refusal was lost: %v", err)
	}
	c := Client{Socket: filepath.Join(t.TempDir(), "missing.sock"), Bundle: "bundle"}
	_, err := c.Resize(context.Background(), "resize", Execution{InstanceID: "instance", IncarnationID: "incarnation", Epoch: 1},
		ResourceChange{Kind: "cpu_grow", CPUs: 2})
	if err == nil || errors.Is(err, rt.ErrUncertain) {
		t.Fatalf("failure before the local send was classified as an uncertain mutation: %v", err)
	}
}
