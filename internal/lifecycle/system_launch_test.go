package lifecycle

import (
	"context"
	"encoding/json"
	"testing"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type systemLaunchRuntime struct {
	nativePowerRuntime
	acceptFirst bool
	wrongRef    bool
	requests    []rt.LaunchSubmission
}

func (n *systemLaunchRuntime) SystemBoot(context.Context) (rt.Capability, error) {
	return rt.Capability{Status: "supported", Supported: true, ContractVersion: 1, VerifiedProfile: rt.SystemBootProfile}, nil
}

func (n *systemLaunchRuntime) DiscoverLaunchBinding(context.Context, string) (rt.LaunchBinding, error) {
	return rt.LaunchBinding{InstanceID: n.execution.InstanceID}, nil
}

func (n *systemLaunchRuntime) SubmitLaunch(_ context.Context, input rt.LaunchSubmission) (rt.OperationReceipt, error) {
	n.requests = append(n.requests, input)
	if n.acceptFirst || len(n.requests) > 1 {
		n.receipt = rt.OperationReceipt{OperationID: input.OperationID, Kind: rt.Launch, RequestDigest: input.RequestDigest,
			RouteID: "deep", InstanceID: input.Expected.InstanceID, BundleDigest: n.route.BundleID,
			Phase: "COMPLETED", Decision: "NONE", Revision: 2, Execution: &n.execution}
		if n.wrongRef {
			foreign := n.execution
			foreign.IncarnationID = "unrelated"
			n.receipt.Execution = &foreign
		}
	}
	if len(n.requests) == 1 {
		return rt.OperationReceipt{}, rt.ErrUnavailable
	}
	return n.receipt, nil
}

func (n *systemLaunchRuntime) QueryOperation(_ context.Context, id string) (rt.OperationReceipt, error) {
	if n.receipt.OperationID != id {
		return rt.OperationReceipt{}, &rt.Error{Code: "NOT_FOUND", NativeCode: "E_NOT_FOUND"}
	}
	return n.receipt, nil
}

func (n *systemLaunchRuntime) NativeInventory(ctx context.Context, ref rt.ExecutionRef) (rt.NativeInventory, error) {
	inventory, err := n.nativePowerRuntime.NativeInventory(ctx, ref)
	inventory.Execution.PID, inventory.Execution.PIDStartTime, inventory.Execution.HostBootID = 12, 345, "boot"
	return inventory, err
}

func TestSystemCreateReconcilesLostLaunchResponse(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		acceptFirst, wrongRef bool
		wantSubmissions       int
	}{
		{"accepted", true, false, 1},
		{"absent", false, false, 2},
		{"different registered execution", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			official := &createRuntime{}
			engine, owner, epoch := runtimeTestEngine(t, ctx, official)
			engine.Config.StateDir = t.TempDir()
			engine.Config.Storage.MinBytes, engine.Config.Storage.MinPercent = 0, 0
			route := rt.BundleRoute{InstanceID: "serve", BundleID: "system-bundle"}
			worker := &systemLaunchRuntime{acceptFirst: tc.acceptFirst, wrongRef: tc.wrongRef}
			worker.route, worker.disposition = route, "RUNNING"
			worker.execution = rt.ExecutionRef{InstanceID: "worker-id", IncarnationID: "vmm-id", ExecutionEpoch: 1}
			engine.runtimes = rt.NewResolver(official, map[rt.BundleRoute]rt.Adapter{route: worker})
			if err := engine.DB.RegisterRuntime(ctx, route, "system-socket", json.RawMessage(`{"guest":{"boot_profiles":["system-debian-v1"]}}`)); err != nil {
				t.Fatal(err)
			}
			tpl := registry.Template{ID: "system", Name: "System", Version: 1, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"debian-root"}`)}
			if err := engine.DB.EnsureTemplate(ctx, tpl); err != nil {
				t.Fatal(err)
			}
			request := Create{Name: "system-machine", TemplateID: tpl.ID, RuntimeInstanceID: route.InstanceID, BootMode: rt.BootSystem, Start: true}
			request.Resources.CPUs, request.Resources.Memory = 2, 1024
			accepted, _, err := engine.DB.Accept(ctx, registry.AcceptRequest{
				Kind: "create", PrincipalID: owner, Epoch: epoch, Key: "system", Scope: "POST /v1/machines", Name: request.Name,
				TemplateID: request.TemplateID, RuntimeInstanceID: route.InstanceID, Request: JSON(request), Heavy: true,
				AdmitRuntime: func(rt.BundleRoute, string) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.drive(ctx, accepted); err != nil {
				t.Fatal(err)
			}
			dispatched, err := engine.DB.GetOperation(ctx, accepted.ID)
			if err != nil || dispatched.State != registry.Observing || dispatched.Effect != registry.EffectUnknown || len(worker.requests) != 1 {
				t.Fatal(dispatched, err)
			}
			sent := worker.requests[0]
			if sent.OperationID != accepted.ID || sent.Expected.ExecutionEpoch != 0 || sent.Expected.IncarnationID != nil ||
				sent.Parameters.BootProfile != rt.SystemBootProfile || sent.Parameters.Init != "/sbin/init" || sent.Parameters.Name != accepted.RuntimeName ||
				len(sent.Parameters.Disks) != 1 || sent.Parameters.Disks[0].GenerationID != "debian-root" || sent.Parameters.Disks[0].ReadOnly ||
				sent.Parameters.Disks[0].AttachmentID != accepted.BindingID+"-root" {
				t.Fatalf("invalid submitted System launch: %+v", sent)
			}
			engine = New(engine.DB, engine.runtimes, engine.Config)
			if err := engine.drive(ctx, dispatched); err != nil {
				t.Fatal(err)
			}
			finished, err := engine.DB.GetOperation(ctx, accepted.ID)
			if err != nil || len(worker.requests) != tc.wantSubmissions || len(official.creates) != 0 {
				t.Fatal(finished, worker.requests, official.creates, err)
			}
			for _, request := range worker.requests {
				if string(JSON(request)) != string(JSON(sent)) {
					t.Fatalf("launch replay changed the durable request: %+v", request)
				}
			}
			machine, err := engine.DB.GetMachine(ctx, accepted.MachineID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wrongRef {
				if finished.State != registry.RecoveryRequired || machine.CurrentBinding != nil {
					t.Fatal(finished, machine)
				}
				return
			}
			binding := machine.CurrentBinding
			if finished.State != registry.Succeeded || finished.Effect != registry.EffectApplied || binding == nil ||
				binding.ExecutionRef() != worker.execution || binding.PowerState != registry.PowerRunning ||
				binding.GuestManagementStatus != "unknown" || binding.Availability == "ready" {
				t.Fatal(finished, binding)
			}
		})
	}
}
