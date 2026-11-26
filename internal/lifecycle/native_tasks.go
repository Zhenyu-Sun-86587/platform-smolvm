package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/tasks"
)

type taskForkStep struct {
	operation string
	source    tasks.Binding
	target    tasks.Binding
}

func (e *Engine) taskForkStep(ctx context.Context, target tasks.Binding) (*taskForkStep, error) {
	step := taskForkStep{target: target}
	var raw sql.NullString
	var sourceID string
	err := e.DB.ReadDB().QueryRowContext(ctx, `SELECT o.id, o.binding_id, json_extract(o.dispatch_json, '$.source_task_binding')
		FROM operation_children c JOIN operations o ON o.id=c.operation_id
		WHERE c.binding_id=? AND c.state='ready'
		UNION ALL
		SELECT o.id, COALESCE(json_extract(o.dispatch_json,'$.source_task_binding.nonce'),captured.binding_id,''),
			json_extract(o.dispatch_json,'$.source_task_binding') FROM operations o
		LEFT JOIN vm_snapshot_provenance v ON v.snapshot_id=json_extract(o.request_json,'$.snapshot_id')
		LEFT JOIN operations captured ON captured.id=v.operation_id
		WHERE o.binding_id=? AND o.kind='vm_restore' AND o.state='succeeded'`, target.Nonce, target.Nonce).Scan(&step.operation, &sourceID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &step.source); err != nil {
			return nil, err
		}
	} else {
		// Older operations retained the immutable source binding ID.
		source, err := e.DB.GetBinding(ctx, sourceID)
		if err != nil {
			return nil, err
		}
		step.source = *GuestBinding(source)
	}
	if step.source.Nonce != sourceID || step.source.MachineID == "" || step.source.Generation < 1 ||
		step.source.Namespace == "" || step.source.Namespace == target.Namespace || step.source.MachineID == target.MachineID {
		return nil, registry.ErrGeneration
	}
	return &step, nil
}

// The guest may have been forked again before its own task index was adopted.
// Replay only transitions recorded for this ancestry, all on the exact child VM.
func (e *Engine) bindNativeTaskIndex(ctx context.Context, b registry.Binding, actual *tasks.Binding, pending string) error {
	target := *GuestBinding(b)
	steps := []taskForkStep{}
	seen := map[string]bool{}
	for actual == nil || target != *actual {
		if seen[target.Nonce] {
			return registry.ErrGeneration
		}
		seen[target.Nonce] = true
		step, err := e.taskForkStep(ctx, target)
		if err != nil {
			return err
		}
		if step == nil {
			if actual != nil {
				return registry.ErrGeneration
			}
			bound, err := e.Call(ctx, b, tasks.Request{Method: "bind", Binding: &target})
			if err != nil {
				return err
			}
			if bound.Binding == nil || *bound.Binding != target {
				return registry.ErrGeneration
			}
			break
		}
		steps = append(steps, *step)
		target = step.source
	}
	// A crash can leave aliases installed and the new binding visible before
	// the guest has committed the transition receipt. Resend its original ID.
	if pending != "" && actual != nil && (len(steps) == 0 || steps[len(steps)-1].operation != pending) {
		step, err := e.taskForkStep(ctx, *actual)
		if err != nil {
			return err
		}
		if step == nil || step.operation != pending {
			return registry.ErrGeneration
		}
		steps = append(steps, *step)
	}
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		reply, err := e.Call(ctx, b, tasks.Request{Method: "adopt_live_fork", OperationID: step.operation,
			Binding: &step.target, Source: &step.source})
		if err != nil {
			return err
		}
		if reply.Binding == nil || *reply.Binding != step.target {
			return registry.ErrGeneration
		}
	}
	return nil
}

type taskIndexEvidence struct {
	Status    string          `json:"task_metadata_status"`
	Execution rt.ExecutionRef `json:"task_execution"`
}

func TaskMetadataStatus(b registry.Binding) string {
	var index taskIndexEvidence
	if json.Unmarshal(b.Health, &index) != nil || index.Status == "" {
		return "unavailable"
	}
	if index.Status == "complete" && (index.Execution != b.ExecutionRef() || b.PowerState != registry.PowerRunning) {
		return "stale"
	}
	return index.Status
}

func (e *Engine) syncNativeTaskIndex(ctx context.Context, b registry.Binding) (*tasks.Hello, error) {
	hello, err := e.Call(ctx, b, tasks.Request{Method: "hello"})
	if err != nil {
		return nil, err
	}
	if hello.Hello == nil || hello.Hello.Protocol != tasks.ProtocolVersion || !hello.Hello.Cgroup {
		return nil, &Failure{"CAPABILITY_NOT_VERIFIED", "Guest Task Manager or cgroup unavailable"}
	}
	expected := GuestBinding(b)
	if hello.Binding == nil || *hello.Binding != *expected || hello.Hello.Transition != "" {
		if err := e.bindNativeTaskIndex(ctx, b, hello.Binding, hello.Hello.Transition); err != nil {
			return nil, err
		}
		hello, err = e.Call(ctx, b, tasks.Request{Method: "hello"})
		if err != nil {
			return nil, err
		}
	}
	if hello.Binding == nil || *hello.Binding != *expected || hello.Hello == nil ||
		hello.Hello.Protocol != tasks.ProtocolVersion || !hello.Hello.Cgroup || hello.Hello.Barrier != nil || hello.Hello.Transition != "" {
		return nil, &Failure{"CAPABILITY_NOT_VERIFIED", "Guest Task Manager binding is not ready"}
	}
	listed, err := e.Call(ctx, b, tasks.Request{Method: "list"})
	if err != nil {
		return nil, err
	}
	status := "complete"
	if hello.Hello.Unknown > 0 {
		status = "stale"
	}
	for _, task := range listed.Tasks {
		if task.ID == "" || task.MachineID != b.MachineID || task.Namespace != b.TaskNamespace || task.Generation != b.Generation {
			return nil, registry.ErrGeneration
		}
		if task.State == "unknown" {
			status = "stale"
		}
	}
	return hello.Hello, e.recordNativeTaskIndex(ctx, b, status, listed.Tasks)
}

func (e *Engine) recordNativeTaskIndex(ctx context.Context, b registry.Binding, status string, items []tasks.Task) error {
	tx, err := e.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var health []byte
	err = tx.QueryRowContext(ctx, `SELECT b.health_json FROM runtime_bindings b JOIN machines m ON m.current_binding_id=b.id
		WHERE b.id=? AND b.incarnation_id=? AND b.execution_epoch=? AND b.power_state='running' AND m.deleted_at IS NULL`,
		b.ID, b.IncarnationID, b.ExecutionEpoch).Scan(&health)
	if err != nil {
		return err
	}
	var previous taskIndexEvidence
	if err := json.Unmarshal(health, &previous); err != nil {
		return err
	}
	observed := status != "unavailable"
	if err := SaveAdopted(ctx, tx, b, items); err != nil {
		return err
	}
	ids := make([]string, 0, len(items))
	for _, task := range items {
		ids = append(ids, task.ID)
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,exit_code=?,observed_at=?,result_json=?
			WHERE id=? AND machine_id=? AND task_namespace=? AND binding_generation=? AND
			(observed_at<=? OR json_extract(result_json,'$.reason')='absent_from_current_guest_task_index')`,
			task.State, task.ExitCode, task.ObservedAt.UnixMilli(), string(JSON(task)), task.ID, b.MachineID,
			b.TaskNamespace, b.Generation, task.ObservedAt.UnixMilli()); err != nil {
			return err
		}
	}
	if observed {
		// A rollback may replace the guest with an earlier journal. Keep the
		// historical row and distinguish a missing runner from a terminal one.
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET state='unknown',observed_at=?,result_json=json_set(result_json,
			'$.state','unknown','$.reason','absent_from_current_guest_task_index','$.observed_at',?)
			WHERE task_namespace=? AND state NOT IN ('succeeded','failed','interrupted','cancelled')
			AND json_extract(result_json,'$.reason') IS NOT 'absent_from_current_guest_task_index'
			AND id NOT IN (SELECT value FROM json_each(?))
			AND NOT EXISTS(SELECT 1 FROM operation_locks l JOIN operations o ON o.id=l.operation_id
				WHERE l.machine_id=? AND o.kind IN ('task_start','task_cancel'))`,
			now.UnixMilli(), now.Format(time.RFC3339Nano), b.TaskNamespace, string(JSON(ids)), b.MachineID)
		if err != nil {
			return err
		}
		var missing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE task_namespace=? AND state='unknown'
			AND json_extract(result_json,'$.reason')='absent_from_current_guest_task_index')`, b.TaskNamespace).Scan(&missing); err != nil {
			return err
		}
		if missing {
			status = "stale"
		}
	} else if previous.Status == "complete" || previous.Status == "stale" {
		status = "stale"
	}
	if next := (taskIndexEvidence{Status: status, Execution: b.ExecutionRef()}); next != previous {
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_bindings SET health_json=json_set(health_json,
			'$.task_metadata_status',?,'$.task_execution',json(?)) WHERE id=?`, status, string(JSON(next.Execution)), b.ID); err != nil {
			return err
		}
		if err := registry.AppendEvent(ctx, tx, "machine", b.MachineID, "", map[string]string{"task_metadata_status": status}, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (e *Engine) sampleNativeTasks(ctx context.Context, b registry.Binding) *tasks.Hello {
	sample, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hello, err := e.syncNativeTaskIndex(sample, b)
	if err != nil {
		_ = e.recordNativeTaskIndex(ctx, b, "unavailable", nil)
		return nil
	}
	return hello
}

// Protect the exact execution from admission until the guest task receipt is
// persisted. The accepted operation owns any wake and the machine lock.
func (j *Job) NativeTaskAccess(ctx context.Context) (func(), error) {
	if bindingRuntime(j.Binding).Legacy() {
		return nil, nil
	}
	if _, ok := PolicyClient(j.Runtime, j.Binding); !ok {
		return nil, rt.ErrUnsupported
	}
	actor := auth.Principal{ID: j.Op.PrincipalID, CredentialID: j.Intent.TaskCredentialID}
	var scope []byte
	err := j.DB.ReadDB().QueryRowContext(ctx, "SELECT scope_json FROM credentials WHERE id=? AND principal_id=?", actor.CredentialID, actor.ID).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(scope, &actor.Scope); err != nil {
		return nil, err
	}
	check := func(ctx context.Context) error {
		m, err := j.DB.GetMachine(ctx, j.Binding.MachineID)
		if err != nil {
			return err
		}
		if m.CurrentBinding == nil || m.CurrentBinding.ID != j.Binding.ID || m.ActiveOperationID != j.Op.ID ||
			m.DeletedAt != nil || (m.CurrentBinding.PowerState == registry.PowerStopped && j.Intent.TaskAcceptedPower != registry.PowerStopped) ||
			(j.Intent.TaskExecution != nil && m.CurrentBinding.ExecutionRef() != *j.Intent.TaskExecution) {
			return registry.ErrGeneration
		}
		return actor.Authorize(auth.ActionManage, m.ID)
	}
	b, release, err := j.AccessAt(ctx, actor, j.Binding.MachineID, AccessEndpoint{Trigger: "api_access", Revision: 1,
		operationID: j.Op.ID, Reauthorize: check})
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		_, err = j.syncNativeTaskIndex(probe, b)
		if err == nil {
			ref := b.ExecutionRef()
			if j.Intent.TaskExecution != nil && ref != *j.Intent.TaskExecution {
				release()
				return nil, registry.ErrGeneration
			}
			j.Binding, j.Intent.TaskExecution = b, &ref
			return release, nil
		}
		if errors.Is(err, registry.ErrGeneration) || probe.Err() != nil {
			release()
			return nil, &Failure{"CAPABILITY_NOT_VERIFIED", "Guest Task Manager binding could not be confirmed"}
		}
		select {
		case <-probe.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}
