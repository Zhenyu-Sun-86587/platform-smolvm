package lifecycle

import (
	"context"
	"errors"
	"time"

	"sandbox-platform/internal/tasks"
)

// CancelGuestTask requires a terminal guest receipt before allowing a power change.
func (j *Job) CancelGuestTask(ctx context.Context, req tasks.Request) (tasks.Task, error) {
	var zero tasks.Task
	if req.GraceSeconds != nil && (*req.GraceSeconds < 0 || *req.GraceSeconds > 300) {
		return zero, &Failure{"INVALID_ARGUMENT", "grace_seconds must be between 0 and 300"}
	}
	req.OperationID = j.Op.ID
	if err := j.Stage(ctx, "guest_cancel", req.TaskID); err != nil {
		return zero, err
	}
	j.Written = true
	grace := 15 * time.Second
	if req.GraceSeconds != nil {
		grace = time.Duration(*req.GraceSeconds+6) * time.Second
	}
	probe, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	for {
		res, err := j.Call(probe, j.Binding, req)
		if err != nil {
			return zero, err
		}
		t := res.Task
		if t == nil || t.ID != req.TaskID || t.Namespace != j.Binding.TaskNamespace || t.MachineID != j.Binding.MachineID || t.Generation != j.Binding.Generation {
			return zero, errors.New("guest cancellation receipt identity mismatch")
		}
		_, err = j.DB.DB().ExecContext(ctx, "UPDATE tasks SET state=?,exit_code=?,observed_at=?,result_json=? WHERE id=? AND task_namespace=? AND binding_generation=?", t.State, t.ExitCode, t.ObservedAt.UnixMilli(), string(JSON(t)), t.ID, t.Namespace, t.Generation)
		if err != nil {
			return zero, err
		}
		if t.Terminal() {
			return *t, nil
		}
		if t.State != "cancelling" {
			return zero, errors.New("guest cancellation outcome requires observation")
		}
		select {
		case <-probe.Done():
			return zero, probe.Err()
		case <-time.After(150 * time.Millisecond):
		}
		req = tasks.Request{Method: "status", TaskID: t.ID}
	}
}
