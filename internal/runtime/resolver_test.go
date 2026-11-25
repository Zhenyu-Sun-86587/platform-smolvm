package runtime

import (
	"errors"
	"testing"
)

type routeAdapter struct{ Adapter }

func TestResolverRoutesOnlyExactBinding(t *testing.T) {
	official := &routeAdapter{}
	custom := &routeAdapter{}
	route := BundleRoute{InstanceID: "serve-a", BundleID: "bundle-v1"}
	resolver := NewResolver(official, map[BundleRoute]Adapter{route: custom})

	for _, test := range []struct {
		name    string
		binding BindingRuntime
		want    Adapter
	}{
		{name: "legacy", want: official},
		{name: "registered instance and bundle", binding: BindingRuntime{BundleRoute: route}, want: custom},
		{name: "different instance", binding: BindingRuntime{BundleRoute: BundleRoute{InstanceID: "serve-b", BundleID: route.BundleID}}},
		{name: "different bundle", binding: BindingRuntime{BundleRoute: BundleRoute{InstanceID: route.InstanceID, BundleID: "bundle-v2"}}},
		{name: "missing bundle", binding: BindingRuntime{BundleRoute: BundleRoute{InstanceID: route.InstanceID}}},
		{name: "execution without route", binding: BindingRuntime{ObjectID: "vm-1", IncarnationID: "exec-2", Epoch: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolver.ForBinding(test.binding)
			if got != test.want || (test.want == nil && !errors.Is(err, ErrUnsupported)) || (test.want != nil && err != nil) {
				t.Fatalf("resolved %p with error %v, want %p", got, err, test.want)
			}
		})
	}
}
