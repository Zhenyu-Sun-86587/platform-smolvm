package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type bindingTransition struct {
	Method      string      `json:"method"`
	OperationID string      `json:"operation_id"`
	Source      Binding     `json:"source"`
	Target      Binding     `json:"target"`
	Policy      string      `json:"policy,omitempty"`
	Fence       string      `json:"fence,omitempty"`
	Aliases     []taskIndex `json:"aliases,omitempty"`
}

func (m *manager) transitionPath() string {
	return filepath.Join(m.j.root, "transition.json")
}

func (m *manager) readTransition() (*bindingTransition, error) {
	var transition bindingTransition
	err := readJSON(m.transitionPath(), &transition)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return &transition, err
}

func (m *manager) adopt(r Request) (Response, error) {
	if !validBinding(r.Binding) || !validBinding(r.Source) || !identifier.MatchString(r.OperationID) ||
		r.Source.Namespace == r.Binding.Namespace || r.Source.Nonce == r.Binding.Nonce {
		return Response{}, rpcError("INVALID_REQUEST", "transition requires distinct source and target identities plus an operation ID")
	}
	if r.Method == "complete_recovery" {
		if r.Binding.MachineID != r.Source.MachineID || r.Binding.Generation <= r.Source.Generation ||
			len(r.StopEvidence) > 512 || strings.TrimSpace(r.StopEvidence) == "" {
			return Response{}, rpcError("INVALID_REQUEST", "recovery requires a newer binding of the source machine and stop evidence")
		}
		if r.Policy == "" {
			r.Policy = "cancel_captured"
		}
		if r.Policy != "cancel_captured" && r.Policy != "resume_captured" {
			return Response{}, rpcError("INVALID_REQUEST", "unknown recovery disposition")
		}
	} else if r.Binding.MachineID == r.Source.MachineID {
		return Response{}, rpcError("INVALID_REQUEST", "copy requires a new logical machine")
	}

	expected := bindingTransition{
		Method: r.Method, OperationID: r.OperationID, Source: *r.Source, Target: *r.Binding,
		Policy: r.Policy, Fence: r.StopEvidence,
	}
	receipts := filepath.Join(m.j.root, "transitions")
	path := filepath.Join(receipts, r.OperationID+".json")
	var committed bindingTransition
	if err := readJSON(path, &committed); err == nil {
		if !sameTransition(committed, expected) || m.binding == nil || *m.binding != committed.Target {
			return Response{}, rpcError("BINDING_MISMATCH", "transition receipt belongs to a different binding")
		}
		if pending, err := m.readTransition(); err != nil {
			return Response{}, err
		} else if pending != nil {
			if !sameTransition(*pending, committed) {
				return Response{}, rpcError("BINDING_MISMATCH", "a different transition is pending")
			}
			if err := removeRecord(m.transitionPath()); err != nil {
				return Response{}, err
			}
		}
		return m.adoptionResult()
	} else if !errors.Is(err, os.ErrNotExist) {
		return Response{}, err
	}
	pending, err := m.readTransition()
	if err != nil {
		return Response{}, err
	}
	if pending != nil && !sameTransition(*pending, expected) {
		return Response{}, rpcError("BINDING_MISMATCH", "a different transition is pending")
	}
	if m.binding == nil || (*m.binding != expected.Source && (pending == nil || *m.binding != expected.Target)) {
		return Response{}, rpcError("BINDING_MISMATCH", "source binding does not match the copied guest")
	}
	if pending == nil {
		if err := m.planTransition(&expected); err != nil {
			return Response{}, err
		}
		if err := writeJSON(m.transitionPath(), expected); err != nil {
			return Response{}, err
		}
		pending = &expected
	}

	if *m.binding == pending.Source {
		if pending.Method != "adopt_live_fork" {
			if err := m.verifyCapture(pending); err != nil {
				return Response{}, err
			}
		}
		if err := m.installAliases(pending); err != nil {
			return Response{}, err
		}
		if err := m.setBinding(pending.Target); err != nil {
			return Response{}, err
		}
	}
	if pending.Method != "complete_copy" {
		if err := m.waitForOwners(pending); err != nil {
			return Response{}, err
		}
	}
	if pending.Method == "complete_recovery" && m.barrier != nil {
		for _, frozen := range m.barrier.Frozen {
			if err := m.disposeFrozen(frozen, pending.OperationID, pending.Policy); err != nil {
				return Response{}, err
			}
		}
	}
	// Persist the receipt before discarding the transition. Repeated calls
	// after response loss return the current view of the same aliases.
	if err := mkdir(receipts); err != nil {
		return Response{}, err
	}
	if m.barrier != nil {
		if err := m.clearBarrier(); err != nil {
			return Response{}, err
		}
	}
	if err := writeJSON(path, pending); err != nil {
		return Response{}, err
	}
	if err := removeRecord(m.transitionPath()); err != nil {
		return Response{}, err
	}
	return m.adoptionResult()
}

func sameTransition(a, b bindingTransition) bool {
	return a.Method == b.Method && a.OperationID == b.OperationID && a.Source == b.Source &&
		a.Target == b.Target && a.Policy == b.Policy && a.Fence == b.Fence
}

func (m *manager) planTransition(t *bindingTransition) error {
	if t.Method == "adopt_live_fork" {
		if m.barrier != nil {
			return rpcError("TASK_CAPTURE_PENDING", "a copied capture barrier needs a recovery disposition")
		}
	} else {
		if err := m.verifyCapture(t); err != nil {
			return err
		}
		if t.Method == "complete_copy" {
			return nil
		}
	}
	indices, err := m.j.taskIndices(t.Source.Namespace)
	if err != nil {
		return err
	}
	for _, index := range indices {
		task, err := m.inspect(t.Source, index)
		if err != nil {
			return err
		}
		if task.Terminal() {
			continue
		}
		t.Aliases = append(t.Aliases, taskIndex{
			ID:    deriveID("task_", t.Target.Namespace, t.Source.MachineID, index.ID),
			RunID: index.RunID, SpecHash: index.SpecHash, OriginTaskID: index.ID, OriginMachineID: t.Source.MachineID,
		})
	}
	return nil
}

func (m *manager) verifyCapture(t *bindingTransition) error {
	if m.barrier == nil || !m.barrier.Ready || m.barrier.Source != t.Source {
		return rpcError("OUTCOME_UNKNOWN", "guest lacks the source capture barrier")
	}
	if t.Method == "complete_copy" && (m.barrier.Content != "clean" || len(m.barrier.Frozen) != 0 ||
		m.barrier.Purpose != "clone" && m.barrier.Purpose != "recovery") {
		return rpcError("TASK_CAPTURE_NOT_CLEAN", "guest was not captured as a clean independent copy")
	}
	for _, frozen := range m.barrier.Frozen {
		state, err := m.frozenState(frozen)
		if err != nil {
			return err
		}
		if !state.frozen || !state.populated {
			return rpcError("OUTCOME_UNKNOWN", "saved task did not remain frozen and populated")
		}
	}
	return nil
}

func (m *manager) installAliases(t *bindingTransition) error {
	if err := mkdir(m.j.indices(t.Target.Namespace)); err != nil {
		return err
	}
	for _, alias := range t.Aliases {
		if err := writeJSON(m.j.index(t.Target.Namespace, alias.ID), alias); err != nil {
			return err
		}
		owner := taskOwner{Binding: t.Target, TaskID: alias.ID, OperationID: t.OperationID}
		if err := writeJSON(m.j.run(alias.RunID, "owner.json"), owner); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) waitForOwners(t *bindingTransition) error {
	deadline := time.Now().Add(time.Second)
	for {
		waiting := false
		for _, alias := range t.Aliases {
			task, err := m.inspect(t.Target, alias)
			if err != nil {
				return err
			}
			if !task.Terminal() && task.Reason == "runner_ownership_unconfirmed" {
				waiting = true
			}
			if t.Method == "complete_recovery" && !task.Terminal() && task.State != "running" &&
				task.State != "cancelling" && task.Reason != "runner_ownership_unconfirmed" {
				return rpcError("OUTCOME_UNKNOWN", "captured runner identity cannot be verified")
			}
		}
		if !waiting {
			return nil
		}
		if time.Now().After(deadline) {
			if t.Method == "adopt_live_fork" {
				// Its native VM is already executing. Publish the logical
				// binding and expose every unacknowledged alias as unknown.
				return nil
			}
			return rpcError("OUTCOME_UNKNOWN", "captured runner did not acknowledge the recovery binding")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (m *manager) adoptionResult() (Response, error) {
	items, err := m.list()
	return Response{Binding: m.binding, Tasks: items}, err
}
