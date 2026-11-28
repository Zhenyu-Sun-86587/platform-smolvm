package nodegateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/runtime/workerclient"
)

func TestWorkerRPCRejectsOtherServeOnSameWorkerAndBundle(t *testing.T) {
	ca := newTestCA(t)
	controller, controllerPin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	node, nodePin := ca.leaf(t, nodeproto.WorkerURI(testAuthority), x509.ExtKeyUsageServerAuth)
	otherRoute := nodeproto.Route{RuntimeInstanceID: "runtime-b", LocalInstanceID: "serve-b", BundleDigest: testRoute.BundleDigest}
	worker := workerclient.Client{Bundle: testRoute.BundleDigest, Socket: "/no-fixture-worker/worker.sock"}
	gateway, err := New(testAuthority, map[nodeproto.Route]Route{
		testRoute:  {Adapter: fake("a"), Worker: worker},
		otherRoute: {Adapter: fake("b"), Worker: worker},
	}, testJournal(t, filepath.Join(t.TempDir(), "journal"), testAuthority))
	if err != nil {
		t.Fatal(err)
	}
	config, err := gateway.TLSConfig(node, ca.roots, controllerPin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(gateway.Handler(controllerPin))
	server.TLS = config
	server.StartTLS()
	defer server.Close()
	client := testClient(t, nodeclient.Config{Endpoint: server.URL, Authority: testAuthority, NodeSPKIPin: nodePin,
		TLS: &tls.Config{Certificates: []tls.Certificate{controller}, RootCAs: ca.roots}})
	a := client.Adapter(testRoute)

	wrong := workerclient.Execution{InstanceID: nodeproto.ManagedInstanceID(otherRoute.LocalInstanceID, "identical-name"),
		IncarnationID: "some-incarnation", Epoch: 11}
	correct := wrong
	correct.InstanceID = nodeproto.ManagedInstanceID(testRoute.LocalInstanceID, "identical-name")
	calls := map[string]func(context.Context, workerclient.Execution) error{
		"policy-read": func(ctx context.Context, ref workerclient.Execution) error {
			return a.Lifecycle(ctx, "GetLifecycleStatus", "", ref, struct{}{}, new(json.RawMessage))
		},
		"policy-mutation": func(ctx context.Context, ref workerclient.Execution) error {
			return a.Lifecycle(ctx, "RequestWake", "operation", ref, struct{}{}, new(json.RawMessage))
		},
		"attachment": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.LookupAttachment(ctx, ref)
			return err
		},
		"packet-endpoint": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.LookupPacketWakeEndpoint(ctx, ref)
			return err
		},
		"packet-rule": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.PacketWakeRule(ctx, ref, workerclient.PacketWakeRule{}, "", "")
			return err
		},
		"resources": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.ComputeResources(ctx, ref)
			return err
		},
		"cpu-limit": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.ManagedCPULimit(ctx, ref)
			return err
		},
		"resize": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.ResizeResources(ctx, "operation", ref, workerclient.ResourceChange{})
			return err
		},
		"set-cpu-limit": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.SetManagedCPULimit(ctx, "operation", ref, nil)
			return err
		},
		"statepoint": func(ctx context.Context, ref workerclient.Execution) error {
			_, err := a.StatePointCapture(ctx, ref, "operation", testDigest, "capture")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var failure *nodeproto.Failure
			if err := call(t.Context(), wrong); !errors.As(err, &failure) || failure.Code != nodeproto.CodeRoute {
				t.Fatalf("cross-Serve execution reached the Worker path: %v", err)
			}
			if err := call(t.Context(), correct); !errors.As(err, &failure) || failure.Code != nodeproto.CodeUnavailable {
				t.Fatalf("correct execution did not query the configured local Worker: %v", err)
			}
		})
	}
}
