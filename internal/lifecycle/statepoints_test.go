package lifecycle

import (
	"context"
	"errors"
	"testing"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type abortedStatePointRuntime struct {
	*forkRuntime
	aborted rt.OperationReceipt
}

func (n *abortedStatePointRuntime) QueryOperation(_ context.Context, id string) (rt.OperationReceipt, error) {
	if id != n.aborted.OperationID {
		return rt.OperationReceipt{}, rt.ErrNotFound
	}
	return n.aborted, nil
}

func (n *abortedStatePointRuntime) StatePointCapability(context.Context, string) (rt.Capability, error) {
	return rt.Capability{}, rt.ErrUnsupported
}

func (n *abortedStatePointRuntime) SystemBoot(context.Context) (rt.Capability, error) {
	return rt.Capability{}, rt.ErrUnsupported
}

func (n *abortedStatePointRuntime) SubmitStatePoint(context.Context, rt.StatePointSubmission) (rt.OperationReceipt, error) {
	n.submissions++
	return rt.OperationReceipt{}, rt.ErrUnsupported
}

func (n *abortedStatePointRuntime) SubmitLaunch(context.Context, rt.LaunchSubmission) (rt.OperationReceipt, error) {
	n.submissions++
	return rt.OperationReceipt{}, rt.ErrUnsupported
}

func TestAbortedStatePointTargetSettlesWithoutResubmission(t *testing.T) {
	for _, tc := range []struct {
		name, kind, decision, effect string
	}{
		{"independent_before_commit", rt.VMRestore, "", registry.EffectNone},
		{"independent_after_commit", rt.VMRestore, "commit", registry.EffectUnknown},
		{"same_machine_after_commit", "start", "commit", registry.EffectUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			engine, original, fork := nativeForkFixture(t, ctx)
			db := engine.DB
			worker := &abortedStatePointRuntime{forkRuntime: original}
			engine.runtimes = rt.NewResolver(engine.OfficialRuntime(), map[rt.BundleRoute]rt.Adapter{worker.route: worker})
			request := registry.AcceptRequest{Kind: tc.kind, Key: "restore-with-exit", Scope: "POST /v1/restore-with-exit",
				PrincipalID: fork.PrincipalID, Epoch: fork.Epoch, AdmitRuntime: engine.AdmitRuntime}
			const capture = "test-saved-state"
			if tc.kind == "start" {
				binding, err := db.GetBinding(ctx, "source-bind")
				if err != nil {
					t.Fatal(err)
				}
				observation := nativeObservation(binding, binding.ExecutionRef(), registry.PowerHibernated, "")
				observation.Native.HibernatedCapture = capture
				observation.RuntimeIdentity = nil
				if err := db.ObserveMachine(ctx, binding.ID, observation); err != nil {
					t.Fatal(err)
				}
				request.MachineID, request.ExpectedGeneration = binding.MachineID, binding.Generation
			} else {
				request.Name, request.TemplateID, request.RuntimeInstanceID = "restored-child", "t1", worker.route.InstanceID
				request.AllocateBinding = true
			}
			accepted, _, err := db.Accept(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			expected := rt.LaunchBinding{InstanceID: "restored-instance"}
			if tc.kind == "start" {
				ref := worker.references["source"]
				expected = rt.LaunchBinding{InstanceID: ref.InstanceID, ExecutionEpoch: ref.ExecutionEpoch}
			}
			intent := rt.StatePointSubmission{Kind: rt.VMRestore, OperationID: accepted.ID, Expected: expected,
				Parameters: rt.StatePointParameters{CaptureID: capture, Mode: rt.RestoreExact, Name: accepted.RuntimeName}}
			intent.RequestDigest = statePointDigest(intent)
			job := &Job{Engine: engine, Op: accepted, Intent: Intent{StatePoint: &intent}}
			if err := job.Stage(ctx, "statepoint_dispatch", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Transition(ctx, accepted.ID, registry.TransitionRequest{State: registry.Observing, Stage: "verify"}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Transition(ctx, accepted.ID, registry.TransitionRequest{State: registry.Failed, Stage: "completed",
				Effect: registry.EffectUnknown}); !errors.Is(err, registry.ErrInvalid) {
				t.Fatalf("unproved terminal unknown effect = %v", err)
			}
			worker.aborted = rt.OperationReceipt{OperationID: accepted.ID, RequestDigest: intent.RequestDigest, Kind: "restore_target",
				RouteID: "deep", BundleDigest: worker.route.BundleID, InstanceID: expected.InstanceID, ExpectedExecutionEpoch: expected.ExecutionEpoch,
				Phase: "ABORTED", Decision: "ABORT", StatePoint: &rt.StatePointProgress{Stage: "aborted", CaptureID: capture,
					ActivationDecision: tc.decision, ErrorCode: "E_RESTORE_ABORTED", Error: "target exited"}}
			observed, err := db.GetOperation(ctx, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.drive(ctx, observed); err != nil {
				t.Fatal(err)
			}
			final, replay, err := db.Accept(ctx, request)
			if err != nil || !replay || final.ID != accepted.ID || final.State != registry.Failed ||
				final.Effect != tc.effect || worker.submissions != 0 {
				t.Fatal(final, replay, worker.submissions, err)
			}
			machine, err := db.GetMachine(ctx, final.MachineID)
			if err != nil || machine.ActiveOperationID != "" {
				t.Fatal(machine, err)
			}
			if tc.kind == "start" {
				if machine.DeletedAt != nil || machine.CurrentBinding == nil || machine.CurrentBinding.PowerState != registry.PowerHibernated ||
					machine.CurrentBinding.LastCaptureID != capture {
					t.Fatal(machine)
				}
			} else if machine.DeletedAt == nil || machine.CurrentBinding != nil {
				t.Fatal(machine)
			}
		})
	}
}
