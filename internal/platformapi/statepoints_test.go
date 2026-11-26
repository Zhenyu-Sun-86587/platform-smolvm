package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type statePointProfileRuntime struct {
	rt.Adapter
	rt.StatePointControl
	requests []string
}

func (runtime *statePointProfileRuntime) StatePointCapability(_ context.Context, kind string) (rt.Capability, error) {
	runtime.requests = append(runtime.requests, kind)
	if kind == rt.VMRollback {
		return rt.FullStateCapabilities(false)["state.rollback"], nil
	}
	return rt.Capability{Status: "supported", Supported: true, VerifiedProfile: "managed-block-v1", ContractVersion: 1}, nil
}

func TestStatePointAdmissionForScopedRelease(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	legacy := rt.BundleRoute{InstanceID: "legacy-serve", BundleID: "legacy-bundle"}
	scoped := rt.BundleRoute{InstanceID: "scoped-serve", BundleID: "scoped-bundle"}
	for route, manifest := range map[rt.BundleRoute]string{
		legacy: `{"contracts":{"state_reader_id":"legacy-reader","managed_statepoint":1}}`,
		scoped: `{"contracts":{"state_reader_id":"scoped-reader","managed_authorized_scope":1}}`,
	} {
		if err := db.RegisterRuntime(ctx, route, route.InstanceID+".sock", json.RawMessage(manifest)); err != nil {
			t.Fatal(err)
		}
	}
	historical, current := &statePointProfileRuntime{}, &statePointProfileRuntime{}
	resolver := rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{legacy: historical, scoped: current})
	s := &Server{Engine: lifecycle.New(db, resolver, config.Defaults())}

	oldBinding := registry.Binding{RuntimeInstanceID: legacy.InstanceID, RuntimeBundleID: legacy.BundleID}
	if err := s.requireStatePointProfile(ctx, oldBinding, rt.VMSnapshot); err != nil || len(historical.requests) != 0 {
		t.Fatalf("legacy admission = %v with handshake requests %v", err, historical.requests)
	}
	binding := registry.Binding{RuntimeInstanceID: scoped.InstanceID, RuntimeBundleID: scoped.BundleID}
	for _, kind := range []string{rt.VMSnapshot, rt.Hibernate, rt.VMRestore} {
		if err := s.requireStatePointProfile(ctx, binding, kind); err != nil {
			t.Fatalf("%s admission = %v", kind, err)
		}
		if last := current.requests[len(current.requests)-1]; last != kind {
			t.Fatalf("%s admission consulted %s", kind, last)
		}
	}
	var capability *rt.CapabilityError
	if err := s.requireStatePointProfile(ctx, binding, rt.VMRollback); !errors.As(err, &capability) ||
		capability.Capability != "state.rollback" || capability.Status.Status != "unknown" {
		t.Fatalf("unmeasured rollback admission = %v", err)
	}
}
