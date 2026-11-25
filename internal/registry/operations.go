package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

const operationColumns = `id, control_epoch, COALESCE(principal_id, ''), COALESCE(machine_id, ''),
	COALESCE(source_machine_id, ''), COALESCE(binding_id, ''), binding_generation, runtime_name,
	kind, state, stage, request_json, COALESCE(dispatch_json, ''), COALESCE(result_json, ''),
	COALESCE(error_json, ''), effect, created_at, updated_at, version, heavy, wake_pending`

func scanOperation(row scanner) (Operation, error) {
	var op Operation
	var request, dispatch, result, failure string
	var created, updated int64
	err := row.Scan(&op.ID, &op.ControlEpoch, &op.PrincipalID, &op.MachineID, &op.SourceMachineID, &op.BindingID,
		&op.BindingGeneration, &op.RuntimeName, &op.Kind, &op.State, &op.Stage, &request, &dispatch, &result, &failure,
		&op.Effect, &created, &updated, &op.Version, &op.Heavy, &op.WakePending)
	op.Request = json.RawMessage(request)
	op.Dispatch = json.RawMessage(dispatch)
	if result != "" {
		op.Result = json.RawMessage(result)
	}
	if failure != "" {
		op.Error = json.RawMessage(failure)
	}
	op.CreatedAt = time.UnixMilli(created).UTC()
	op.UpdatedAt = time.UnixMilli(updated).UTC()
	return op, notFound(err)
}

func readOperation(ctx context.Context, q queryer, id string) (Operation, error) {
	return scanOperation(q.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM operations WHERE id = ?", id))
}

func (s *Store) GetOperation(ctx context.Context, id string) (Operation, error) {
	return readOperation(ctx, s.reader, id)
}

func (s *Store) ListPending(ctx context.Context) ([]Operation, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT "+operationColumns+
		" FROM operations WHERE state IN ('queued', 'dispatching', 'observing', 'recovery_required') ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ops := []Operation{}
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func (s *Store) Accept(ctx context.Context, r AcceptRequest) (Operation, bool, error) {
	envelope := r.mutation()
	if err := validateMutation(envelope); err != nil {
		return Operation{}, false, err
	}
	if !validateLabel(r.Kind, 100) {
		return Operation{}, false, fmt.Errorf("%w: operation kind is required", ErrInvalid)
	}
	creates := oneOf(r.Kind, "create", "fork", "branch", "clone", "restore")
	fromSource := creates && r.Kind != "create" && r.MachineID != ""
	if creates && (!validateLabel(r.Name, 255) || r.Kind == "create" && r.MachineID != "" ||
		oneOf(r.Kind, "fork", "branch") && !fromSource) {
		return Operation{}, false, fmt.Errorf("%w: a new machine name and eligible source are required", ErrInvalid)
	}

	r.Locks = slices.Clone(r.Locks)
	sort.Slice(r.Locks, func(i, j int) bool { return r.Locks[i].MachineID < r.Locks[j].MachineID })
	for i, lock := range r.Locks {
		if lock.MachineID == "" || lock.ExpectedGeneration < 0 || (i > 0 && lock.MachineID == r.Locks[i-1].MachineID) {
			return Operation{}, false, fmt.Errorf("%w: invalid or repeated operation lock target", ErrInvalid)
		}
	}
	request, err := ensureJSON(r.Request, "{}")
	if err != nil {
		return Operation{}, false, err
	}
	idle, err := ensureJSON(r.IdlePolicy, "{}")
	if err != nil {
		return Operation{}, false, err
	}
	idleCanonical, err := canonicalJSON([]byte(idle))
	if err != nil {
		return Operation{}, false, err
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback()
	target := struct {
		Kind, MachineID, Name, TemplateID, DesiredPower string
		IdlePolicy                                      json.RawMessage
		KeepRunning, Heavy, Allocate                    bool
		Locks                                           []Lock
	}{r.Kind, r.MachineID, r.Name, r.TemplateID, r.DesiredPower, idleCanonical,
		r.KeepRunning, r.Heavy, r.AllocateBinding, r.Locks}
	fingerprint, err := mutationFingerprint(ctx, tx, envelope, target)
	if err != nil {
		return Operation{}, false, err
	}
	existing, found, err := readIdempotency(ctx, tx, envelope)
	if err != nil {
		return Operation{}, false, err
	}
	if found {
		if existing.Hash != fingerprint || existing.OperationID == "" {
			return Operation{}, false, ErrIdempotency
		}
		op, err := readOperation(ctx, tx, existing.OperationID)
		return op, true, err
	}
	if err := checkWritable(ctx, tx); err != nil {
		return Operation{}, false, err
	}

	id, err := NewID("op_")
	if err != nil {
		return Operation{}, false, err
	}
	now := time.Now().UTC()
	op := Operation{
		ID: id, ControlEpoch: r.Epoch, PrincipalID: r.PrincipalID, Kind: r.Kind, MachineID: r.MachineID,
		State: Queued, Stage: "accepted", Effect: EffectNone, Request: json.RawMessage(request),
		CreatedAt: now, UpdatedAt: now, Version: 1, Heavy: r.Heavy,
	}
	var prior Machine
	if r.MachineID != "" {
		prior, err = readMachine(ctx, tx, r.MachineID)
		if err != nil {
			return Operation{}, false, err
		}
		if err := checkMachineExpected(prior, r.ExpectedRevision, r.ExpectedGeneration); err != nil {
			return Operation{}, false, err
		}
	}
	if creates {
		if fromSource {
			op.SourceMachineID = prior.ID
			if r.TemplateID == "" {
				r.TemplateID = prior.TemplateID
			}
		}
		if _, err := readTemplate(ctx, tx, r.TemplateID); err != nil {
			return Operation{}, false, err
		}
		op.MachineID, err = NewID("m_")
		if err != nil {
			return Operation{}, false, err
		}
		desired := r.DesiredPower
		if desired == "" {
			desired = "stopped"
		}
		spec := request
		if fromSource {
			spec = string(prior.Spec)
			if len(r.IdlePolicy) == 0 {
				idle = string(prior.IdlePolicy)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO machines
			(id, name, template_id, revision, binding_generation, desired_power, spec_json, idle_policy_json, keep_running, last_user_activity_at)
			VALUES (?, ?, ?, 1, 0, ?, ?, ?, ?, ?)`, op.MachineID, r.Name, r.TemplateID, desired, spec, idle, r.KeepRunning, now.UnixMilli()); err != nil {
			return Operation{}, false, uniqueConflict(err)
		}
	}

	locks := make(map[string]Lock)
	if op.MachineID != "" {
		generation := prior.BindingGeneration
		if creates {
			generation = 0
		}
		locks[op.MachineID] = Lock{MachineID: op.MachineID, ExpectedGeneration: generation, Role: "target"}
	}
	if fromSource {
		locks[prior.ID] = Lock{MachineID: prior.ID, ExpectedGeneration: prior.BindingGeneration, Role: "source"}
	}
	for _, lock := range r.Locks {
		if automatic, ok := locks[lock.MachineID]; ok {
			if lock.ExpectedGeneration != 0 && lock.ExpectedGeneration != automatic.ExpectedGeneration {
				return Operation{}, false, ErrGeneration
			}
			continue
		}
		target, err := readMachine(ctx, tx, lock.MachineID)
		if err != nil {
			return Operation{}, false, err
		}
		if err := checkMachineExpected(target, 0, lock.ExpectedGeneration); err != nil {
			return Operation{}, false, err
		}
		lock.ExpectedGeneration = target.BindingGeneration
		if lock.Role == "" {
			lock.Role = "related"
		}
		locks[lock.MachineID] = lock
	}

	if creates || r.AllocateBinding {
		if op.MachineID == "" {
			return Operation{}, false, fmt.Errorf("%w: binding allocation requires a machine", ErrInvalid)
		}
		if err := allocateBinding(ctx, tx, &op, request); err != nil {
			return Operation{}, false, err
		}
	} else if prior.CurrentBinding != nil {
		op.BindingGeneration = prior.BindingGeneration
		op.BindingID = prior.CurrentBinding.ID
		op.RuntimeName = prior.CurrentBinding.RuntimeName
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO operations
		(id, control_epoch, principal_id, machine_id, source_machine_id, binding_id, binding_generation, runtime_name,
		kind, state, stage, request_json, effect, created_at, updated_at, heavy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, op.ID, op.ControlEpoch, op.PrincipalID, nullable(op.MachineID),
		nullable(op.SourceMachineID), nullable(op.BindingID), op.BindingGeneration, op.RuntimeName, op.Kind, op.State, op.Stage,
		request, op.Effect, now.UnixMilli(), now.UnixMilli(), op.Heavy)
	if err != nil {
		return Operation{}, false, err
	}
	machineIDs := make([]string, 0, len(locks))
	for machineID := range locks {
		machineIDs = append(machineIDs, machineID)
	}
	sort.Strings(machineIDs)
	for _, machineID := range machineIDs {
		lock := locks[machineID]
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_locks (machine_id, operation_id, expected_generation, role)
			VALUES (?, ?, ?, ?)`, machineID, op.ID, lock.ExpectedGeneration, lock.Role); err != nil {
			return Operation{}, false, err
		}
		if prior.ID == machineID {
			if _, err := tx.ExecContext(ctx, "UPDATE machines SET revision = revision + 1 WHERE id = ?", machineID); err != nil {
				return Operation{}, false, err
			}
		}
		if err := appendMachineEvent(ctx, tx, machineID, op.ID, op.Kind == "delete"); err != nil {
			return Operation{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency (control_epoch, principal_id, scope, key, request_hash, operation_id)
		VALUES (?, ?, ?, ?, ?, ?)`, r.Epoch, r.PrincipalID, r.Scope, r.Key, fingerprint, op.ID); err != nil {
		return Operation{}, false, err
	}
	if err := appendOperationEvent(ctx, tx, op); err != nil {
		return Operation{}, false, err
	}
	return op, false, tx.Commit()
}

func checkMachineExpected(m Machine, revision, generation int64) error {
	if m.DeletedAt != nil {
		return ErrNotFound
	}
	if m.ActiveOperationID != "" {
		return &ActiveOperationError{MachineID: m.ID, OperationID: m.ActiveOperationID}
	}
	if revision != 0 && revision != m.Revision {
		return ErrRevision
	}
	if generation != 0 && generation != m.BindingGeneration {
		return ErrGeneration
	}
	return nil
}

func allocateBinding(ctx context.Context, tx *sql.Tx, op *Operation, request string) error {
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(generation), 0) + 1 FROM runtime_bindings WHERE machine_id = ?",
		op.MachineID).Scan(&op.BindingGeneration); err != nil {
		return err
	}
	var err error
	op.BindingID, err = NewID("bind_")
	if err != nil {
		return err
	}
	op.RuntimeName, err = NewID("sbox_")
	if err != nil {
		return err
	}
	namespace, err := NewID("ns_")
	if err != nil {
		return err
	}
	canonical, err := canonicalJSON(json.RawMessage(request))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_bindings
		(id, machine_id, generation, runtime_name, status, task_namespace, spec_hash)
		VALUES (?, ?, ?, ?, 'candidate', ?, ?)`, op.BindingID, op.MachineID, op.BindingGeneration,
		op.RuntimeName, namespace, hex.EncodeToString(digest[:]))
	return err
}

func (s *Store) Transition(ctx context.Context, id string, r TransitionRequest) (Operation, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	op, err := s.TransitionTx(ctx, tx, id, r)
	if err != nil {
		return Operation{}, err
	}
	return op, tx.Commit()
}

func (s *Store) TransitionTx(ctx context.Context, tx *sql.Tx, id string, r TransitionRequest) (Operation, error) {
	op, err := readOperation(ctx, tx, id)
	if err != nil {
		return Operation{}, err
	}
	if r.ExpectedVersion != 0 && r.ExpectedVersion != op.Version {
		return Operation{}, ErrOperationState
	}
	if !allowedTransition(op, r.State) {
		return Operation{}, fmt.Errorf("%w: %s to %s", ErrOperationState, op.State, r.State)
	}
	if r.Stage == "" {
		return Operation{}, fmt.Errorf("%w: operation stage is required", ErrInvalid)
	}
	if r.State == Dispatching {
		if err := checkDispatch(ctx, tx, op, r); err != nil {
			return Operation{}, err
		}
	}
	machineIDs, err := verifyOperationLocks(ctx, tx, op)
	if err != nil {
		return Operation{}, err
	}
	var binding Binding
	if op.BindingID != "" {
		binding, err = readBinding(ctx, tx, op.BindingID)
		if err != nil {
			return Operation{}, err
		}
		if binding.MachineID != op.MachineID || binding.Generation != op.BindingGeneration || binding.RuntimeName != op.RuntimeName ||
			binding.Status == "retired" {
			return Operation{}, ErrGeneration
		}
	}
	if r.Observation != nil {
		if binding.ID == "" {
			return Operation{}, fmt.Errorf("%w: no operation binding to observe", ErrInvalid)
		}
		if _, err := persistObservation(ctx, tx, binding, *r.Observation); err != nil {
			return Operation{}, err
		}
	}
	if r.PromoteBinding {
		if r.State != Succeeded || r.MarkDeleted || binding.ID == "" || !oneOf(binding.Status, "candidate", "unknown") {
			return Operation{}, fmt.Errorf("%w: only a successful eligible candidate can become current", ErrInvalid)
		}
		if err := promoteBinding(ctx, tx, binding); err != nil {
			return Operation{}, err
		}
	}
	if r.MarkDeleted {
		if r.State != Succeeded || op.Kind != "delete" || op.MachineID == "" {
			return Operation{}, fmt.Errorf("%w: deletion requires a successful delete operation", ErrInvalid)
		}
		if err := deleteMachine(ctx, tx, op.MachineID); err != nil {
			return Operation{}, err
		}
	}
	if r.DesiredPower != "" && op.MachineID != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE machines SET desired_power = ? WHERE id = ? AND deleted_at IS NULL", r.DesiredPower, op.MachineID); err != nil {
			return Operation{}, err
		}
	}
	if r.Apply != nil {
		if err := r.Apply(ctx, tx); err != nil {
			return Operation{}, err
		}
	}

	if r.Effect != "" {
		op.Effect = r.Effect
	}
	if r.State == RecoveryRequired {
		op.Effect = EffectUnknown
	}
	if !oneOf(op.Effect, EffectNone, EffectApplied, EffectPartial, EffectUnknown) ||
		(oneOf(r.State, Succeeded, Failed, Cancelled) && op.Effect == EffectUnknown) ||
		(r.State == Cancelled && op.Effect != EffectNone) {
		return Operation{}, fmt.Errorf("%w: invalid effect for operation state", ErrInvalid)
	}
	for _, raw := range []json.RawMessage{r.Dispatch, r.Result, r.Error} {
		if len(raw) > 0 && !json.Valid(raw) {
			return Operation{}, fmt.Errorf("%w: invalid operation evidence JSON", ErrInvalid)
		}
	}
	if len(r.Dispatch) > 0 {
		op.Dispatch = r.Dispatch
	}
	if len(r.Result) > 0 {
		op.Result = r.Result
	}
	if len(r.Error) > 0 {
		op.Error = r.Error
	}
	if r.WakePending != nil {
		op.WakePending = *r.WakePending
	}
	if r.State == Succeeded && len(r.Error) == 0 {
		op.Error = nil
	}
	op.State = r.State
	op.Stage = r.Stage
	op.UpdatedAt = time.Now().UTC()
	op.Version++

	_, err = tx.ExecContext(ctx, `UPDATE operations SET state = ?, stage = ?, effect = ?, dispatch_json = ?,
		result_json = ?, error_json = ?, updated_at = ?, version = ?, wake_pending = ? WHERE id = ?`,
		op.State, op.Stage, op.Effect, rawValue(op.Dispatch), rawValue(op.Result), rawValue(op.Error),
		op.UpdatedAt.UnixMilli(), op.Version, op.WakePending, op.ID)
	if err != nil {
		return Operation{}, err
	}
	if r.State == RecoveryRequired && binding.Status == "candidate" {
		if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET status = 'unknown' WHERE id = ?", binding.ID); err != nil {
			return Operation{}, err
		}
	}
	if op.Terminal() {
		if !r.PromoteBinding && oneOf(binding.Status, "candidate", "unknown") {
			if op.State == Succeeded {
				return Operation{}, fmt.Errorf("%w: successful operation has an uncommitted candidate", ErrInvalid)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET status = 'retired' WHERE id = ?", binding.ID); err != nil {
				return Operation{}, err
			}
			if op.Effect == EffectNone && op.BindingGeneration == 1 {
				if err := deleteMachine(ctx, tx, op.MachineID); err != nil {
					return Operation{}, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM operation_locks WHERE operation_id = ?", op.ID); err != nil {
			return Operation{}, err
		}
	}
	if err := appendOperationEvent(ctx, tx, op); err != nil {
		return Operation{}, err
	}
	if r.Observation != nil || r.DesiredPower != "" || op.Terminal() {
		for _, machineID := range machineIDs {
			if err := appendMachineEvent(ctx, tx, machineID, op.ID, op.Kind == "delete"); err != nil {
				return Operation{}, err
			}
		}
	}
	return op, nil
}

func allowedTransition(op Operation, next string) bool {
	switch op.State {
	case Queued:
		return oneOf(next, Dispatching, Cancelled) && (next != Cancelled || !op.WakePending)
	case Dispatching:
		return oneOf(next, Dispatching, Observing, RecoveryRequired)
	case Observing:
		return oneOf(next, Observing, Succeeded, Failed, RecoveryRequired)
	case RecoveryRequired:
		return next == Observing
	default:
		return false
	}
}

func checkDispatch(ctx context.Context, tx *sql.Tx, op Operation, request TransitionRequest) error {
	if err := checkWritable(ctx, tx); err != nil {
		return err
	}
	if len(request.Dispatch) == 0 && len(op.Dispatch) == 0 {
		return fmt.Errorf("%w: dispatch requires durable outgoing intent", ErrInvalid)
	}
	if op.State != Queued || !op.Heavy {
		return nil
	}
	limit := request.MaxHeavy
	if limit <= 0 {
		limit = 1
	}
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations
		WHERE heavy = 1 AND state IN ('dispatching', 'observing', 'recovery_required')`).Scan(&count)
	if err != nil {
		return err
	}
	if count >= limit {
		return ErrHeavyBusy
	}
	return nil
}

func verifyOperationLocks(ctx context.Context, tx *sql.Tx, op Operation) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.machine_id, l.expected_generation, m.binding_generation
		FROM operation_locks l JOIN machines m ON m.id = l.machine_id WHERE l.operation_id = ? ORDER BY l.machine_id`, op.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var machines []string
	for rows.Next() {
		var machineID string
		var expected, actual int64
		if err := rows.Scan(&machineID, &expected, &actual); err != nil {
			return nil, err
		}
		if expected != actual {
			return nil, ErrGeneration
		}
		machines = append(machines, machineID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if (op.MachineID != "" && !slices.Contains(machines, op.MachineID)) ||
		(op.SourceMachineID != "" && !slices.Contains(machines, op.SourceMachineID)) {
		return nil, ErrGeneration
	}
	return machines, nil
}

func promoteBinding(ctx context.Context, tx *sql.Tx, b Binding) error {
	if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET status = 'retired' WHERE machine_id = ? AND status = 'current'", b.MachineID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET status = 'current' WHERE id = ?", b.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE machines SET current_binding_id = ?, binding_generation = ?, revision = revision + 1
		WHERE id = ? AND deleted_at IS NULL`, b.ID, b.Generation, b.MachineID)
	return err
}

func deleteMachine(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE runtime_bindings SET status = 'retired' WHERE machine_id = ? AND status = 'current'`, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE machines SET current_binding_id = NULL,
		deleted_at = ?, revision = revision + 1, desired_power = 'deleted' WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UnixMilli(), id)
	return err
}

func (s *Store) Cancel(ctx context.Context, id string) (Operation, error) {
	return s.Transition(ctx, id, TransitionRequest{State: Cancelled, Stage: "completed", Effect: EffectNone})
}

// ActiveOperation reads the durable owner of a machine mutation lock.
func (s *Store) ActiveOperation(ctx context.Context, machineID string) (Operation, error) {
	var id string
	err := s.reader.QueryRowContext(ctx, "SELECT operation_id FROM operation_locks WHERE machine_id = ?", machineID).Scan(&id)
	if err != nil {
		return Operation{}, notFound(err)
	}
	return readOperation(ctx, s.reader, id)
}

// HeavyInFlight reports dispatched heavy operations whose effects remain in
// flight or ambiguous, including across process restarts.
func (s *Store) HeavyInFlight(ctx context.Context) (int, error) {
	var count int
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations
		WHERE heavy = 1 AND state IN ('dispatching', 'observing', 'recovery_required')`).Scan(&count)
	return count, err
}

var _ = errors.Is
