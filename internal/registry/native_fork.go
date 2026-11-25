package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	rt "sandbox-platform/internal/runtime"
)

func validateForkChildren(r AcceptRequest) error {
	if !rt.IsNativeFork(r.Kind) {
		if len(r.ForkChildren) != 0 {
			return fmt.Errorf("%w: child set requires a native fork", ErrInvalid)
		}
		return nil
	}
	if r.MachineID == "" || len(r.ForkChildren) == 0 || len(r.ForkChildren) > rt.MaxForkChildren ||
		(r.Kind == rt.IndependentFork && len(r.ForkChildren) != 1) {
		return fmt.Errorf("%w: native fork requires a source and an eligible child count", ErrInvalid)
	}
	names := map[string]bool{}
	for _, name := range r.ForkChildren {
		if !validateLabel(name, 255) || names[name] {
			return fmt.Errorf("%w: every fork child needs a distinct name", ErrInvalid)
		}
		names[name] = true
	}
	return nil
}

func allocateForkChildren(ctx context.Context, tx *sql.Tx, op *Operation, source Machine, names []string, route rt.BundleRoute, locks map[string]Lock) error {
	if source.CurrentBinding == nil || route == (rt.BundleRoute{}) || op.BindingID != source.CurrentBinding.ID {
		return ErrGeneration
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(source.Spec, &spec); err != nil || spec == nil {
		return fmt.Errorf("%w: invalid source workspace specification", ErrInvalid)
	}
	for ordinal, name := range names {
		mid, err := NewID("m_")
		if err != nil {
			return err
		}
		spec["name"], _ = json.Marshal(name)
		childSpec, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO machines
			(id, name, template_id, revision, binding_generation, desired_power, spec_json, idle_policy_json, keep_running, last_user_activity_at)
			VALUES (?, ?, ?, 1, 0, 'stopped', ?, ?, ?, ?)`, mid, name, source.TemplateID, string(childSpec),
			string(source.IdlePolicy), source.KeepRunning, time.Now().UnixMilli())
		if err != nil {
			return uniqueConflict(err)
		}
		child := Operation{MachineID: mid}
		if err := allocateBinding(ctx, tx, &child, string(childSpec), route); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO operation_children
			(operation_id, ordinal, binding_id, state, resource_independence) VALUES (?, ?, ?, 'pending', 'unknown')`, op.ID, ordinal, child.BindingID)
		if err != nil {
			return err
		}
		op.Children = append(op.Children, OperationChild{Ordinal: ordinal, MachineID: mid, BindingID: child.BindingID, State: "pending", ResourceIndependence: "unknown"})
		if err = SeedLifecycle(ctx, tx, mid, source.ID, op.Request, op.ID); err != nil {
			return err
		}
		locks[mid] = Lock{MachineID: mid, Role: "child"}
	}
	return nil
}

func advanceNativeFork(ctx context.Context, tx *sql.Tx, op *Operation, r TransitionRequest) ([]string, error) {
	if !rt.IsNativeFork(op.Kind) {
		if r.ReleaseSourceLock || len(r.ForkChildren) != 0 || len(r.ForkRetryOrdinals) != 0 {
			return nil, fmt.Errorf("%w: fork evidence outside a native fork", ErrInvalid)
		}
		return nil, nil
	}
	var events []string
	if r.ReleaseSourceLock && !op.SourceLockReleased {
		if r.SourceDisposition != SourceRunning && r.SourceDisposition != SourceStandby {
			return nil, fmt.Errorf("%w: releasing a fork source requires its stable disposition", ErrInvalid)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM operation_locks WHERE machine_id=? AND operation_id=?", op.MachineID, op.ID); err != nil {
			return nil, err
		}
		op.SourceLockReleased = true
		events = append(events, op.MachineID)
	}
	for _, ordinal := range r.ForkRetryOrdinals {
		if !op.SourceLockReleased || op.State != Observing || r.State != Observing || ordinal < 0 || ordinal >= len(op.Children) {
			return nil, ErrOperationState
		}
		child := &op.Children[ordinal]
		if err := reopenForkChild(ctx, tx, op.ID, child); err != nil {
			return nil, err
		}
		events = append(events, child.MachineID)
	}
	for _, change := range r.ForkChildren {
		if change.Ordinal < 0 || change.Ordinal >= len(op.Children) || op.Children[change.Ordinal].Ordinal != change.Ordinal ||
			(!op.SourceLockReleased && change.State == "ready") {
			return nil, fmt.Errorf("%w: invalid native fork child transition", ErrInvalid)
		}
		child := &op.Children[change.Ordinal]
		if err := applyForkChild(ctx, tx, op.ID, child, change); err != nil {
			return nil, err
		}
		events = append(events, child.MachineID)
	}
	if !oneOf(r.State, Succeeded, Failed, Cancelled) {
		return events, nil
	}
	for i := range op.Children {
		child := &op.Children[i]
		if child.State != "pending" {
			continue
		}
		if r.State == Succeeded || op.SourceLockReleased || r.Effect != EffectNone {
			return nil, fmt.Errorf("%w: fork completion requires every child outcome", ErrInvalid)
		}
		change := ForkChildTransition{Ordinal: child.Ordinal, State: "quarantined", ErrorCode: "FORK_NOT_STARTED"}
		if r.State == Cancelled {
			change.ErrorCode = "FORK_CANCELLED"
		}
		if err := applyForkChild(ctx, tx, op.ID, child, change); err != nil {
			return nil, err
		}
		events = append(events, child.MachineID)
	}
	return events, nil
}

func verifiedForkCancellation(op Operation, r TransitionRequest) bool {
	return rt.IsNativeFork(op.Kind) && op.State == Observing && r.State == Cancelled && r.Effect == EffectNone &&
		r.Stage == "completed" && len(r.ForkChildren) == len(op.Children)
}

func reopenForkChild(ctx context.Context, tx *sql.Tx, operationID string, child *OperationChild) error {
	if child.State != "quarantined" || child.ErrorCode != "NATIVE_CHILD_ABORTED" {
		return ErrOperationState
	}
	updated, err := tx.ExecContext(ctx, `UPDATE machines SET deleted_at=NULL,desired_power='stopped',revision=revision+1
		WHERE id=? AND deleted_at IS NOT NULL AND binding_generation=0 AND current_binding_id IS NULL`, child.MachineID)
	if err != nil {
		return err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		return ErrGeneration
	}
	updated, err = tx.ExecContext(ctx, `UPDATE runtime_bindings SET status='candidate'
		WHERE id=? AND machine_id=? AND status='retired' AND generation=1 AND execution_epoch IS NULL`, child.BindingID, child.MachineID)
	if err != nil {
		return err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		return ErrGeneration
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operation_locks (machine_id,operation_id,expected_generation,role)
		VALUES(?,?,0,'child')`, child.MachineID, operationID); err != nil {
		return err
	}
	child.State, child.ResourceIndependence, child.ErrorCode = "pending", "unknown", ""
	child.Attempt++
	_, err = tx.ExecContext(ctx, `UPDATE operation_children SET state='pending',resource_independence='unknown',error_code=NULL,attempt=?
		WHERE operation_id=? AND ordinal=?`, child.Attempt, operationID, child.Ordinal)
	return err
}

// BeginNativeForkRetryTx journals a retry of a settled, released cut. The
// failed children stay hidden until the Worker confirms a new target attempt.
func (s *Store) BeginNativeForkRetryTx(ctx context.Context, tx *sql.Tx, id string, version int64, dispatch json.RawMessage) (Operation, error) {
	op, err := readOperation(ctx, tx, id)
	if err != nil {
		return op, err
	}
	if op.Version != version {
		return Operation{}, ErrRevision
	}
	if !rt.IsNativeFork(op.Kind) || !op.SourceLockReleased ||
		!oneOf(op.State, Succeeded, Failed) || !oneOf(op.Effect, EffectNone, EffectPartial) || !json.Valid(dispatch) {
		return Operation{}, ErrOperationState
	}
	retryable := false
	for _, child := range op.Children {
		retryable = retryable || (child.State == "quarantined" && child.ErrorCode == "NATIVE_CHILD_ABORTED")
	}
	if !retryable {
		return Operation{}, ErrOperationState
	}
	op.State, op.Stage, op.Dispatch, op.Error = Observing, "fork_retry", dispatch, nil
	op.Version++
	op.UpdatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, `UPDATE operations SET state=?,stage=?,dispatch_json=?,error_json=NULL,version=?,updated_at=? WHERE id=?`,
		op.State, op.Stage, string(op.Dispatch), op.Version, op.UpdatedAt.UnixMilli(), op.ID)
	if err != nil {
		return Operation{}, err
	}
	return op, appendOperationEvent(ctx, tx, op)
}

func applyForkChild(ctx context.Context, tx *sql.Tx, operationID string, child *OperationChild, change ForkChildTransition) error {
	if child.State != "pending" {
		return ErrOperationState
	}
	binding, err := readBinding(ctx, tx, child.BindingID)
	if err != nil {
		return err
	}
	if binding.Status != "candidate" || binding.MachineID != child.MachineID || binding.Generation != 1 {
		return ErrGeneration
	}
	proof := []byte("[]")
	switch change.State {
	case "ready":
		if change.Observation == nil || change.Observation.Native == nil || change.Observation.Native.PowerState != PowerRunning ||
			len(change.ResourceProof) == 0 || change.ErrorCode != "" {
			return fmt.Errorf("%w: publication requires independent live child evidence", ErrInvalid)
		}
		if _, err := persistObservation(ctx, tx, binding, *change.Observation); err != nil {
			return err
		}
		if err := promoteBinding(ctx, tx, binding); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE machines SET desired_power='running' WHERE id=?", child.MachineID); err != nil {
			return err
		}
		proof, err = json.Marshal(change.ResourceProof)
		if err != nil {
			return err
		}
		child.ResourceProof = change.ResourceProof
		child.ResourceIndependence = "verified"
	case "quarantined":
		if change.ErrorCode == "" || change.Observation != nil {
			return fmt.Errorf("%w: quarantined child requires a confirmed error", ErrInvalid)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET status='retired' WHERE id=?", binding.ID); err != nil {
			return err
		}
		if err := deleteMachine(ctx, tx, binding.MachineID); err != nil {
			return err
		}
		child.ResourceIndependence = "failed"
	default:
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM operation_locks WHERE machine_id=? AND operation_id=?", child.MachineID, operationID); err != nil {
		return err
	}
	child.State, child.ErrorCode = change.State, change.ErrorCode
	_, err = tx.ExecContext(ctx, `UPDATE operation_children SET state=?, resource_independence=?, error_code=?, resource_proof_json=?
		WHERE operation_id=? AND ordinal=?`, child.State, child.ResourceIndependence, nullable(child.ErrorCode), string(proof), operationID, child.Ordinal)
	return err
}
