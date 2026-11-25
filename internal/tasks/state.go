package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func (m *manager) inspect(binding Binding, index taskIndex) (Task, error) {
	var request taskRequest
	if err := readJSON(m.j.run(index.RunID, "request.json"), &request); err != nil {
		return Task{}, err
	}
	if request.RunID != index.RunID || request.SpecHash != index.SpecHash {
		return Task{}, errors.New("task index does not match request")
	}
	t := Task{
		ID: index.ID, RunID: index.RunID, MachineID: binding.MachineID,
		Namespace: binding.Namespace, Generation: binding.Generation,
		OriginTaskID: index.OriginTaskID, OriginMachineID: index.OriginMachineID,
		CreatedAt: request.CreatedAt, ObservedAt: time.Now().UTC(),
	}
	for stream, log := range map[string]*LogIndex{"stdout": &t.Stdout, "stderr": &t.Stderr} {
		err := readJSON(m.j.run(index.RunID, stream, "index.json"), log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Task{}, err
		}
	}
	terminal, err := m.readResult(index)
	if err != nil {
		return Task{}, err
	}
	if terminal != nil {
		t.State, t.Reason, t.ExitCode, t.Signal = terminal.State, terminal.Reason, terminal.ExitCode, terminal.Signal
		t.Stdout, t.Stderr = terminal.Stdout, terminal.Stderr
		return t, nil
	}
	if request.BootID != m.boot {
		t.State, t.Reason = "interrupted", "guest_cold_boot"
		return t, nil
	}

	var identity Identity
	err = readJSON(m.j.run(index.RunID, "runner.json"), &identity)
	switch {
	case errors.Is(err, os.ErrNotExist):
		var dispatch taskRequest
		err := readJSON(m.j.run(index.RunID, "dispatch.json"), &dispatch)
		switch {
		case errors.Is(err, os.ErrNotExist):
			t.State = "accepted"
		case err != nil:
			return Task{}, err
		case time.Since(dispatch.CreatedAt) < 3*time.Second:
			t.State = "starting"
		default:
			t.State, t.Reason = "unknown", "dispatch_without_runner"
		}
	case err != nil:
		return Task{}, err
	default:
		t.Runner = &identity
		observed, err := processStart(identity.PID)
		switch {
		case err != nil || observed != identity.StartTime:
			t.State, t.Reason = "unknown", "runner_identity_lost"
		case verifyCgroup(m.root, t.RunID, m.boot, identity) != nil:
			t.State, t.Reason = "unknown", "payload_cgroup_identity_lost"
		case identity.Released:
			t.State = "running"
		default:
			t.State = "starting"
		}
		if t.State == "running" && index.OriginTaskID != "" {
			var ack ownerAck
			err := readJSON(m.j.run(t.RunID, "owner-ack.json"), &ack)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return Task{}, err
			}
			if err != nil || ack.Owner.Binding != binding || ack.Owner.TaskID != index.ID || ack.Identity != identity {
				t.State, t.Reason = "unknown", "runner_ownership_unconfirmed"
			}
		}
		if t.State != "unknown" {
			for _, marker := range []string{"cancel.json", "output-limit.json"} {
				var cancellation cancelIntent
				err := readJSON(m.j.run(t.RunID, marker), &cancellation)
				if err == nil {
					t.State, t.Reason = "cancelling", cancellation.Reason
					break
				}
				if !errors.Is(err, os.ErrNotExist) {
					return Task{}, err
				}
			}
		}
	}
	// A result may land between the initial receipt read and identity check.
	terminal, err = m.readResult(index)
	if err != nil {
		return Task{}, err
	}
	if terminal != nil {
		t.State, t.Reason, t.ExitCode, t.Signal = terminal.State, terminal.Reason, terminal.ExitCode, terminal.Signal
		t.Stdout, t.Stderr = terminal.Stdout, terminal.Stderr
	}
	return t, nil
}

func (m *manager) readResult(index taskIndex) (*result, error) {
	var terminal result
	err := readJSON(m.j.run(index.RunID, "result.json"), &terminal)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !(Task{State: terminal.State}).Terminal() {
		return nil, errors.New("invalid runner terminal state")
	}
	return &terminal, nil
}

func (m *manager) cancel(r Request, index taskIndex, t Task) (Response, error) {
	if t.Terminal() {
		return Response{Task: &t}, nil
	}
	if m.barrier != nil {
		return Response{}, rpcError("TASK_CAPTURE_PENDING", "a capture operation owns task disposition")
	}
	grace := 10
	if r.GraceSeconds != nil {
		grace = *r.GraceSeconds
	}
	if grace < 0 || grace > 300 || !identifier.MatchString(r.OperationID) {
		return Response{}, rpcError("INVALID_REQUEST", "cancel requires a valid operation ID and grace up to 300 seconds")
	}
	if t.Runner == nil {
		return Response{Task: &t}, rpcError("OUTCOME_UNKNOWN", "payload identity cannot be verified")
	}
	if err := verifyCgroup(m.root, t.RunID, m.boot, *t.Runner); err != nil {
		return Response{Task: &t}, err
	}
	intent := cancelIntent{OperationID: r.OperationID, KillAt: time.Now().Add(time.Duration(grace) * time.Second).UTC(), Reason: "cancel_requested"}
	path := m.j.run(t.RunID, "cancel.json")
	var existing cancelIntent
	if err := readJSON(path, &existing); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON(path, intent); err != nil {
			return Response{}, err
		}
		if grace > 0 {
			// A failed TERM cannot block the durable cgroup.kill deadline.
			if err := termCgroup(t.Runner.Cgroup); err != nil {
				slog.Warn("task TERM incomplete", "run_id", t.RunID, "error", err)
			}
		}
	} else if err != nil {
		return Response{}, err
	} else {
		intent = existing
	}
	if err := m.driveCancel(t, intent); err != nil {
		return Response{Task: &t}, err
	}
	t, err := m.inspect(*m.binding, index)
	return Response{Task: &t}, err
}

func (m *manager) driveCancel(t Task, intent cancelIntent) error {
	if t.Terminal() || time.Now().Before(intent.KillAt) {
		return nil
	}
	if t.Runner == nil {
		return errors.New("cancellation cannot verify payload cgroup")
	}
	if err := verifyCgroup(m.root, t.RunID, m.boot, *t.Runner); err != nil {
		if terminal, readErr := m.readResult(taskIndex{RunID: t.RunID}); readErr == nil && terminal != nil {
			return nil
		}
		return err
	}
	state, err := readCgroup(t.Runner.Cgroup)
	if err != nil {
		return err
	}
	if state.populated {
		return killCgroup(t.Runner.Cgroup)
	}
	return nil
}

func (m *manager) reconcile(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			err := m.advanceCancellations()
			m.mu.Unlock()
			if err != nil {
				slog.Warn("task cancellation reconciliation", "error", err)
			}
		}
	}
}

func (m *manager) advanceCancellations() error {
	if m.binding == nil || m.barrier != nil {
		return nil
	}
	indexes, err := m.j.taskIndices(m.binding.Namespace)
	if err != nil {
		return err
	}
	var failures error
	for _, index := range indexes {
		var intent cancelIntent
		err := readJSON(m.j.run(index.RunID, "cancel.json"), &intent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && time.Now().After(intent.KillAt) {
			var task Task
			task, err = m.inspect(*m.binding, index)
			if err == nil {
				err = m.driveCancel(task, intent)
			}
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", index.ID, err))
		}
	}
	return failures
}
