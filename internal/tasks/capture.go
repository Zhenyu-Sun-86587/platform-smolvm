package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *manager) barrierPath() string {
	return filepath.Join(m.j.root, "barrier.json")
}

func (m *manager) prepareCapture(r Request) (Response, error) {
	if !identifier.MatchString(r.OperationID) {
		return Response{}, rpcError("INVALID_REQUEST", "capture requires an operation ID")
	}
	purpose := "recovery"
	if r.Method == "prepare_copy" {
		purpose = "clone"
		if r.Policy != "managed_clean" && r.Policy != "" {
			return Response{}, rpcError("INVALID_REQUEST", "copy capture requires managed_clean")
		}
	}

	if m.barrier == nil {
		capture, err := m.planCapture(r.OperationID, purpose)
		if err != nil {
			return Response{}, err
		}
		if err := writeJSON(m.barrierPath(), capture); err != nil {
			return Response{}, err
		}
		m.barrier = capture
	}
	if m.barrier.OperationID != r.OperationID || m.barrier.Source != *m.binding || m.barrier.Purpose != purpose {
		return Response{}, rpcError("TASK_CAPTURE_PENDING", "a different capture owns the task barrier")
	}

	for _, frozen := range m.barrier.Frozen {
		state, err := m.frozenState(frozen)
		if err != nil {
			return Response{}, err
		}
		if m.barrier.Ready && !state.frozen {
			return Response{}, rpcError("OUTCOME_UNKNOWN", "captured task lost its freezer state")
		}
		if !state.frozen {
			if err := setFreezer(frozen.Identity.Cgroup, true); err != nil {
				return Response{}, err
			}
		}
		// An exit racing the freeze is indeterminate until the runner's
		// durable result can be read and a new capture can be planned.
		if err := waitCgroup(frozen.Identity.Cgroup, func(s cgroupState) bool { return s.frozen && s.populated }); err != nil {
			return Response{}, err
		}
	}
	if !m.barrier.Ready {
		capture := *m.barrier
		processes, err := m.captureProcesses(capture.Frozen)
		if err != nil {
			return Response{}, err
		}
		capture.Processes = processes
		capture.Ready = true
		if err := writeJSON(m.barrierPath(), &capture); err != nil {
			return Response{}, err
		}
		m.barrier = &capture
	}
	return Response{Capture: m.barrier, Binding: m.binding}, nil
}

func (m *manager) planCapture(operationID, purpose string) (*Capture, error) {
	if err := m.startGate(); err != nil {
		return nil, err
	}
	capture := &Capture{OperationID: operationID, Purpose: purpose, Source: *m.binding, Content: "clean"}
	tasks, err := m.list()
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if task.Terminal() {
			continue
		}
		if purpose == "clone" {
			return nil, rpcError("TASK_CAPTURE_NOT_CLEAN", "guest has an unfinished task")
		}
		if task.State != "running" || task.Runner == nil {
			return nil, rpcError("OUTCOME_UNKNOWN", "guest has an unfinished task without a verifiable running payload")
		}
		frozen := FrozenTask{ID: task.ID, RunID: task.RunID, Identity: *task.Runner}
		state, err := m.frozenState(frozen)
		if err != nil {
			return nil, err
		}
		if state.frozen || !state.populated {
			return nil, rpcError("OUTCOME_UNKNOWN", "payload was not populated and thawed before capture")
		}
		capture.Content = "active"
		capture.Frozen = append(capture.Frozen, frozen)
	}
	if _, err := m.captureProcesses(capture.Frozen); err != nil {
		return nil, err
	}
	return capture, nil
}

func (m *manager) finishCapture(r Request) (Response, error) {
	if !identifier.MatchString(r.OperationID) {
		return Response{}, rpcError("INVALID_REQUEST", "capture requires an operation ID")
	}
	if m.barrier == nil {
		return Response{Binding: m.binding}, nil
	}
	if r.OperationID != m.barrier.OperationID || *m.binding != m.barrier.Source {
		return Response{}, rpcError("TASK_CAPTURE_PENDING", "capture is owned by a different source operation")
	}
	for _, frozen := range m.barrier.Frozen {
		state, err := m.frozenState(frozen)
		if errors.Is(err, os.ErrNotExist) {
			if receipt, readErr := m.readResult(taskIndex{RunID: frozen.RunID}); readErr == nil && receipt != nil {
				continue
			}
		}
		if err != nil {
			return Response{}, err
		}
		if state.frozen {
			if err := setFreezer(frozen.Identity.Cgroup, false); err != nil {
				return Response{}, err
			}
		}
	}
	return Response{Binding: m.binding}, m.clearBarrier()
}

func (m *manager) frozenState(frozen FrozenTask) (cgroupState, error) {
	if err := verifyCgroup(m.root, frozen.RunID, m.boot, frozen.Identity); err != nil {
		return cgroupState{}, fmt.Errorf("task %s: %w", frozen.ID, err)
	}
	return readCgroup(frozen.Identity.Cgroup)
}

func setFreezer(path string, frozen bool) error {
	value := "0"
	if frozen {
		value = "1"
	}
	if err := writeCgroup(path, "cgroup.freeze", value); err != nil {
		return err
	}
	return waitCgroup(path, func(s cgroupState) bool { return s.frozen == frozen })
}

func (m *manager) clearBarrier() error {
	if err := removeRecord(m.barrierPath()); err != nil {
		return err
	}
	m.barrier = nil
	return nil
}

func (m *manager) disposeFrozen(frozen FrozenTask, operationID, policy string) error {
	if receipt, err := m.readResult(taskIndex{RunID: frozen.RunID}); err != nil {
		return err
	} else if receipt != nil {
		if policy == "cancel_captured" && receipt.Reason != "interrupted_by_restore" {
			return errors.New("captured task exited without the recovery cancellation receipt")
		}
		return nil
	}
	state, err := m.frozenState(frozen)
	if err != nil {
		return err
	}
	if policy == "resume_captured" {
		if state.frozen {
			return setFreezer(frozen.Identity.Cgroup, false)
		}
		return nil
	}
	if state.populated && !state.frozen {
		return errors.New("captured payload must remain frozen through cancellation")
	}
	intent := cancelIntent{OperationID: operationID, Reason: "interrupted_by_restore", KillAt: time.Now().UTC()}
	if err := writeJSON(m.j.run(frozen.RunID, "cancel.json"), intent); err != nil {
		return err
	}
	if state.populated {
		if err := killCgroup(frozen.Identity.Cgroup); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		// The runner only publishes this receipt after confirming that
		// the cgroup is empty and both output streams are durable.
		receipt, err := m.readResult(taskIndex{RunID: frozen.RunID})
		if err != nil {
			return err
		}
		if receipt != nil {
			if receipt.Reason != intent.Reason {
				return errors.New("captured runner produced a different disposition")
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("captured runner did not confirm cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
