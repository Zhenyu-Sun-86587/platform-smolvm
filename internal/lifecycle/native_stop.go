package lifecycle

import (
	"context"
	"database/sql"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (j *Job) confirmNativeStop(ctx context.Context, receipt rt.OperationReceipt) error {
	control, ok := j.Runtime.(rt.NativeStopControl)
	progress := receipt.Native
	if !ok || progress.Stage != "complete" || progress.SourceDisposition != string(registry.SourceExited) ||
		progress.Mode != "managed" || receipt.Decision != "NONE" || receipt.Execution != nil {
		return &Failure{"RECOVERY_REQUIRED", "Worker did not confirm the stopped execution and writer fence"}
	}
	source := j.Intent.Native.Expected
	current, err := control.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if current.InstanceID != source.InstanceID || current.ExecutionEpoch != source.ExecutionEpoch || current.IncarnationID != nil {
		return registry.ErrStaleObservation
	}
	observation := nativeObservation(j.Binding, source, registry.PowerStopped, "")
	observation.Native.VerifiedStop = true
	observation.RuntimeIdentity = nil
	observation.Ports = JSON([]rt.Port{})
	observation.GuestManagementEvidence = JSON(map[string]any{})
	return j.Done(ctx, registry.TransitionRequest{
		Observation: &observation, Effect: registry.EffectApplied, SourceDisposition: registry.SourceExited, DesiredPower: "stopped",
		Result: JSON(map[string]any{"machine_id": j.Op.MachineID, "execution_epoch": source.ExecutionEpoch}),
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='interrupted',observed_at=?,result_json=
				json_set(result_json,'$.state','interrupted','$.reason','execution_stopped_after_worker_fence','$.observed_at',?)
				WHERE machine_id=? AND binding_generation=? AND task_namespace=?
				AND state NOT IN ('succeeded','failed','cancelled','interrupted')`,
				now.UnixMilli(), now.Format(time.RFC3339Nano), j.Op.MachineID, j.Binding.Generation, j.Binding.TaskNamespace); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE exec_sessions SET state='lost',last_observed_at=? WHERE binding_id=?
				AND state NOT IN ('failed','exited','done','lost')`, now.UnixMilli(), j.Binding.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE leases SET state='released',revision=revision+1,release_reason='execution stopped'
				WHERE machine_id=? AND binding_generation=? AND state!='released'
				AND (worker_revision IS NULL OR incarnation_id=? AND execution_epoch=?)`,
				j.Op.MachineID, j.Binding.Generation, source.IncarnationID, source.ExecutionEpoch)
			return err
		},
	})
}
