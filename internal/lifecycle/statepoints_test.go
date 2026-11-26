package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
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
		name, kind, decision, state, effect string
	}{
		{"independent_before_commit", rt.VMRestore, "", registry.Failed, registry.EffectNone},
		{"independent_after_commit", rt.VMRestore, "commit", registry.Failed, registry.EffectUnknown},
		{"same_machine_after_commit", "start", "commit", registry.Failed, registry.EffectUnknown},
		{"unknown_decision", rt.VMRestore, "unrecognized", registry.RecoveryRequired, registry.EffectUnknown},
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
			if err != nil || !replay || final.ID != accepted.ID || final.State != tc.state ||
				final.Effect != tc.effect || worker.submissions != 0 {
				t.Fatal(final, replay, worker.submissions, err)
			}
			if tc.state == registry.RecoveryRequired {
				return
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

func TestCheckpointPersistsConfigurationFromDispatch(t *testing.T) {
	ctx := context.Background()
	engine, _, request := nativeForkFixture(t, ctx)
	db := engine.DB
	if _, err := db.DB().ExecContext(ctx, `UPDATE runtime_bundles SET manifest_json='{"contracts":{"state_reader_id":"reader"}}' WHERE id='managed';
		UPDATE machines SET spec_json='{"boot_mode":"system","resources":{"cpu_count":1,"memory_mib":512},"idle_policy":{"enabled":true}}',revision=5 WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	request.Kind, request.Key, request.Scope = rt.VMSnapshot, "snapshot-a", "POST /v1/machines/source/snapshots"
	request.Request = JSON(map[string]string{"capture_mode": "vm"})
	request.ForkChildren, request.AdmitNative = nil, nil
	request.AdmitRuntime = func(route rt.BundleRoute, kind string) error {
		if route.BundleID != "managed" || kind != rt.VMSnapshot {
			return rt.ErrUnsupported
		}
		return nil
	}
	op, _, err := db.Accept(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetBinding(ctx, op.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := db.GetMachine(ctx, op.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{Engine: engine, Op: op, Binding: binding}
	dispatch, err := job.prepareStatePoint(ctx, nil, rt.VMSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	job.Intent.StatePoint = &dispatch
	if err := job.Stage(ctx, "statepoint_dispatch", ""); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the job from durable intent as on a process restart.
	job = &Job{Engine: engine, Op: job.Op, Binding: binding}
	if err := json.Unmarshal(job.Op.Dispatch, &job.Intent); err != nil {
		t.Fatal(err)
	}
	point := rt.StatePointProgress{CaptureID: dispatch.Parameters.CaptureID, RepresentationID: "representation-a", ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		OriginalSourceDisposition: "RUNNING", Phase: "durable", SourceReleased: true}
	if err := job.completeCheckpoint(ctx, point); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE machines SET spec_json='{"boot_mode":"system","resources":{"cpu_count":4,"memory_mib":2048}}',revision=10 WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	catalog := snapshots.Catalog{DB: db}
	item, err := catalog.Get(ctx, "snap_"+strings.TrimPrefix(op.ID, "op_"))
	if err != nil {
		t.Fatal(err)
	}
	config := snapshots.CapturedConfiguration(item)
	if config == nil || config.SourceMachineRevision != source.Revision || string(config.Spec) != `{"template_id":"t1","boot_mode":"system","resources":{"cpu_count":1,"memory_mib":512}}` {
		t.Fatalf("wrong capture config: %+v", config)
	}
}
