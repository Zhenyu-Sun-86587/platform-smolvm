package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const retireInstanceMethod = "RetireStoppedInstance"

type systemRetirement struct {
	Execution workerclient.Execution `json:"expected"`
	Digest    string                 `json:"request_digest"`
}

func (e *Engine) AdmitNativeDelete(b registry.Binding, _ string) error {
	if b.PowerState != registry.PowerStopped {
		return &Failure{"STATE_CONFLICT", "stop the System machine before deleting it"}
	}
	adapter, err := e.ForBinding(b)
	if err != nil {
		return err
	}
	if _, ok := PolicyClient(adapter, b); !ok {
		return rt.ErrUnsupported
	}
	return nil
}

func (j *Job) driveSystemDelete(ctx context.Context, routeError error) error {
	launcher, supported := j.Runtime.(rt.NativeLauncher)
	client, managed := PolicyClient(j.Runtime, j.Binding)
	if routeError != nil || !supported || !managed {
		if j.Op.State == registry.Queued {
			if err := j.Stage(ctx, "preflight", ""); err != nil {
				return err
			}
		}
		j.Written = j.Intent.Retirement != nil
		if routeError == nil {
			routeError = rt.ErrUnsupported
		}
		return j.fail(ctx, routeError)
	}
	if err := readyRuntime(ctx, j.Runtime); err != nil {
		return err
	}
	args := map[string]string{"name": j.Binding.RuntimeName}
	if j.Intent.Retirement == nil {
		if err := j.prepareDispatch(ctx); err != nil || j.Op.Terminal() {
			return err
		}
		if err := j.preflightSystemDelete(ctx, launcher); err != nil {
			if errors.Is(err, rt.ErrUnavailable) {
				return err
			}
			return j.fail(ctx, err)
		}
		intent := systemRetirement{Execution: WorkerExecution(j.Binding)}
		var err error
		intent.Digest, err = workerclient.LifecycleDigest(retireInstanceMethod, j.Op.ID, intent.Execution, args)
		if err != nil {
			return err
		}
		j.Intent.Retirement = &intent
		if err := j.Stage(ctx, "worker_retirement", "absent"); err != nil {
			return err
		}
	}
	j.Written = true
	intent := j.Intent.Retirement
	digest, err := workerclient.LifecycleDigest(retireInstanceMethod, j.Op.ID, intent.Execution, args)
	if err != nil || intent.Digest != digest || intent.Execution.IncarnationID != "" ||
		intent.Execution.InstanceID != j.Binding.RuntimeInstanceObjectID || intent.Execution.Epoch != j.Binding.ExecutionEpoch {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "retirement intent differs from the pinned System binding"})
	}
	receipt, err := launcher.QueryOperation(ctx, j.Op.ID)
	if launchReceiptMissing(err) {
		prior, authErr := j.checkGuestSubmission(ctx, launcher.QueryOperation)
		if authErr != nil || j.Op.Terminal() {
			return authErr
		}
		if prior != nil {
			receipt, err = *prior, nil
		} else {
			err = client.Lifecycle(ctx, retireInstanceMethod, j.Op.ID, intent.Execution, args, &receipt)
			if refusal := retirementRefusal(err); refusal != nil {
				j.Written = false
				return j.fail(ctx, refusal)
			}
		}
	}
	if err != nil {
		return err
	}
	if receipt.OperationID != j.Op.ID || receipt.Kind != "retire_instance" || receipt.RequestDigest != intent.Digest ||
		receipt.RouteID != "deep" || receipt.BundleDigest != j.Binding.RuntimeBundleID || receipt.InstanceID != intent.Execution.InstanceID ||
		receipt.ExpectedExecutionEpoch != intent.Execution.Epoch || receipt.ExpectedIncarnationID != nil || receipt.Execution != nil || receipt.Decision != "NONE" {
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker retirement receipt differs from the accepted intent"})
	}
	switch receipt.Phase {
	case "NEW":
		if j.Op.State == registry.Observing && j.Op.Stage == "worker_retirement" && j.Op.Effect == registry.EffectUnknown {
			return nil
		}
		return j.transition(ctx, registry.TransitionRequest{State: registry.Observing, Stage: "worker_retirement", Effect: registry.EffectUnknown})
	case "COMPLETED":
		return j.finishDelete(ctx, &receipt)
	default:
		return j.fail(ctx, &Failure{"RECOVERY_REQUIRED", "Worker retirement phase is unknown"})
	}
}

func retirementRefusal(err error) error {
	var fault *workerclient.Fault
	if !errors.As(err, &fault) {
		return nil
	}
	return nativeRefusal(&rt.Error{NativeCode: fault.Code, Message: fault.Message})
}

func (j *Job) preflightSystemDelete(ctx context.Context, launcher rt.NativeLauncher) error {
	if err := j.AdmitNativeDelete(j.Binding, "delete"); err != nil {
		return err
	}
	machine, err := j.DB.GetMachine(ctx, j.Op.MachineID)
	if err != nil {
		return err
	}
	var spec Create
	if err := json.Unmarshal(machine.Spec, &spec); err != nil {
		return err
	}
	if spec.BootMode != rt.BootSystem {
		return rt.ErrUnsupported
	}
	var dependent bool
	if err := j.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dependencies
		WHERE parent_binding_id=? AND kind='live_branch_resource' AND status!='released')`, j.Binding.ID).Scan(&dependent); err != nil {
		return err
	}
	if dependent {
		return &Failure{"DEPENDENCY_BLOCKED", "live fork child retains a native resource dependency"}
	}
	actual, err := launcher.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if actual.InstanceID != j.Binding.RuntimeInstanceObjectID || actual.ExecutionEpoch != j.Binding.ExecutionEpoch || actual.IncarnationID != nil {
		return registry.ErrStaleObservation
	}
	return nil
}

func (j *Job) finishDelete(ctx context.Context, receipt *rt.OperationReceipt) error {
	finish := registry.TransitionRequest{MarkDeleted: true, Effect: registry.EffectApplied, DesiredPower: "deleted"}
	if receipt != nil {
		finish.SourceDisposition = registry.SourceExited
		finish.Result = JSON(map[string]any{"machine_id": j.Op.MachineID, "execution_epoch": receipt.ExpectedExecutionEpoch})
	}
	finish.Apply = func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE dependencies SET status='released' WHERE child_binding_id=? AND kind='live_branch_resource'", j.Binding.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE port_allocations SET state='released' WHERE binding_id=?", j.Binding.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_pins WHERE owner_kind='binding' AND owner_id=?
			OR owner_kind='pre_replacement' AND owner_id IN (SELECT id FROM operations WHERE machine_id=? AND kind='rollback')`, j.Binding.ID, j.Binding.MachineID); err != nil {
			return err
		}
		if receipt == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE volume_attachments SET state='detached',revision=revision+1
			WHERE binding_id=? AND state='reserved'`, j.Binding.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET state='released',revision=revision+1,release_reason='instance retired'
			WHERE machine_id=? AND binding_generation=? AND state!='released'`, j.Op.MachineID, j.Binding.Generation); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE lifecycle_lease_intents SET state='desktop_closed',secret=x''
			WHERE binding_id=? AND state IN ('desktop_pending','desktop_sent','desktop_applied')`, j.Binding.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE services SET enabled=0,revision=revision+1 WHERE machine_id=? AND enabled=1`, j.Op.MachineID)
		return err
	}
	return j.Done(ctx, finish)
}
