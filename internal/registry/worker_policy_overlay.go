package registry

import (
	"context"
	"database/sql"
	"encoding/json"

	rt "sandbox-platform/internal/runtime"
)

type WorkerPolicyOverlay struct {
	WorkerID               string
	BindingID              string
	MachineID              string
	MaintenanceRevision    int64
	DesiredRevision        int64
	RecordedWorkerRevision int64
	Effective              rt.LifecyclePolicy
}

// WorkerPolicyPlan derives temporary effective policy from the current desired
// policy and the Worker's current maintenance revision.
func readWorkerPolicyPlan(ctx context.Context, q queryer, binding Binding) (WorkerPolicyOverlay, bool, error) {
	state, clearing, err := readWorkerMaintenance(ctx, q, binding.WorkerID)
	if err != nil {
		return WorkerPolicyOverlay{}, false, err
	}
	draining := oneOf(state.Mode, "draining", "drained")
	if !draining && !clearing {
		return WorkerPolicyOverlay{}, false, nil
	}
	policy, err := readPolicy(ctx, q, binding.MachineID)
	if err != nil {
		return WorkerPolicyOverlay{}, false, err
	}
	if policy.BindingID != binding.ID {
		return WorkerPolicyOverlay{}, false, ErrGeneration
	}
	plan := WorkerPolicyOverlay{WorkerID: binding.WorkerID, BindingID: binding.ID, MachineID: binding.MachineID,
		MaintenanceRevision: state.Revision, DesiredRevision: policy.DesiredRevision, Effective: policy.Desired}
	if draining {
		plan.Effective.Wake = rt.WakeConditions{}
	}
	err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT worker_revision FROM worker_policy_overlays
		WHERE binding_id=? AND maintenance_revision=? AND desired_revision=?),0)`, binding.ID,
		plan.MaintenanceRevision, plan.DesiredRevision).Scan(&plan.RecordedWorkerRevision)
	return plan, true, err
}

func (s *Store) WorkerPolicyPlan(ctx context.Context, binding Binding) (WorkerPolicyOverlay, bool, error) {
	if binding.WorkerID == "" {
		binding.WorkerID = LocalWorkerID
	}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkerPolicyOverlay{}, false, err
	}
	defer tx.Rollback()
	plan, managed, err := readWorkerPolicyPlan(ctx, tx, binding)
	if err != nil {
		return plan, managed, err
	}
	return plan, managed, tx.Commit()
}

// RecordWorkerPolicyOverlay projects the Worker's wire revision onto the
// logical policy revision after rechecking both CAS preconditions.
func (s *Store) RecordWorkerPolicyOverlay(ctx context.Context, plan WorkerPolicyOverlay, workerRevision int64) (LifecycleSettings, error) {
	if workerRevision <= 0 {
		return LifecycleSettings{}, ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleSettings{}, err
	}
	defer tx.Rollback()
	binding, err := readBinding(ctx, tx, plan.BindingID)
	if err != nil {
		return LifecycleSettings{}, err
	}
	now, managed, err := readWorkerPolicyPlan(ctx, tx, binding)
	if err != nil {
		return LifecycleSettings{}, err
	}
	if !managed || now.WorkerID != plan.WorkerID || now.MachineID != plan.MachineID ||
		now.MaintenanceRevision != plan.MaintenanceRevision || now.DesiredRevision != plan.DesiredRevision ||
		now.Effective != plan.Effective || now.RecordedWorkerRevision > workerRevision {
		return LifecycleSettings{}, ErrRevision
	}
	raw, err := json.Marshal(plan.Effective)
	if err != nil {
		return LifecycleSettings{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_policy_overlays VALUES(?,?,?,?,?,?)
		ON CONFLICT(binding_id) DO UPDATE SET maintenance_revision=excluded.maintenance_revision,
		desired_revision=excluded.desired_revision,worker_revision=excluded.worker_revision,effective_json=excluded.effective_json`,
		plan.BindingID, plan.WorkerID, plan.MaintenanceRevision, plan.DesiredRevision, workerRevision, string(raw)); err != nil {
		return LifecycleSettings{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE machine_lifecycle_policies SET applied_revision=?,applied_json=?,last_error=NULL WHERE machine_id=?`,
		plan.DesiredRevision, string(raw), plan.MachineID); err != nil {
		return LifecycleSettings{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lifecycle_policy_application_targets SET state='applied'
		WHERE state='pending' AND machine_id=? AND binding_id=? AND policy_revision=?`,
		plan.MachineID, plan.BindingID, plan.DesiredRevision); err != nil {
		return LifecycleSettings{}, err
	}
	policy, err := readPolicy(ctx, tx, plan.MachineID)
	if err != nil {
		return LifecycleSettings{}, err
	}
	return policy, tx.Commit()
}
