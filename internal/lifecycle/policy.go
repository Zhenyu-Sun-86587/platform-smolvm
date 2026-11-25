package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"time"
)

type WorkerLifecycleStatus struct {
	StatePoint  string   `json:"statepoint"`
	Instance    string   `json:"instance_id"`
	Incarnation string   `json:"incarnation_id"`
	Epoch       int64    `json:"execution_epoch"`
	Revision    int64    `json:"binding_revision"`
	Applied     int64    `json:"applied_revision"`
	Power       string   `json:"power"`
	Suspended   bool     `json:"controlled_suspended"`
	Blockers    []string `json:"blockers"`
	Collectors  map[string]struct {
		Session string `json:"session"`
		Seq     int64  `json:"seq"`
	} `json:"collectors"`
	Decision *struct {
		ID     string `json:"operation_id"`
		Origin string `json:"origin"`
		Kind   string `json:"kind"`
		Stage  string `json:"stage"`
	} `json:"last_decision"`
}

func PolicyClient(adapter rt.Adapter, b registry.Binding) (workerclient.Client, bool) {
	p, ok := adapter.(interface{ LifecycleWorkerSocket() string })
	if !ok || p.LifecycleWorkerSocket() == "" || b.RuntimeInstanceObjectID == "" || bindingRuntime(b).Legacy() {
		return workerclient.Client{}, false
	}
	return workerclient.Client{Socket: p.LifecycleWorkerSocket(), Bundle: b.RuntimeBundleID}, true
}
func WorkerExecution(b registry.Binding) workerclient.Execution {
	return workerclient.Execution{InstanceID: b.RuntimeInstanceObjectID, IncarnationID: b.IncarnationID, Epoch: b.ExecutionEpoch}
}
func (s WorkerLifecycleStatus) Execution() workerclient.Execution {
	return workerclient.Execution{InstanceID: s.Instance, IncarnationID: s.Incarnation, Epoch: s.Epoch}
}
func (e *Engine) ApplyMachinePolicy(ctx context.Context, b registry.Binding, client workerclient.Client) (registry.LifecycleSettings, error) {
	p, err := e.DB.LifecyclePolicy(ctx, b.MachineID)
	if err != nil || p.AppliedRevision == p.DesiredRevision {
		return p, err
	}
	key := fmt.Sprintf("policy-%s-%d", b.MachineID, p.DesiredRevision)
	encoded := JSON(p.Desired)
	ref := JSON(WorkerExecution(b))
	_, err = e.DB.DB().ExecContext(ctx, `INSERT OR IGNORE INTO lifecycle_deliveries(id,machine_id,binding_id,desired_revision,policy_json,expected_json)VALUES(?,?,?,?,?,?)`,
		key, b.MachineID, b.ID, p.DesiredRevision, string(encoded), string(ref))
	if err != nil {
		return p, err
	}
	var binding, raw, expected string
	err = e.DB.ReadDB().QueryRowContext(ctx, "SELECT binding_id,policy_json,expected_json FROM lifecycle_deliveries WHERE id=?", key).Scan(&binding, &raw, &expected)
	if err != nil {
		return p, err
	}
	if b.ID != binding {
		return p, registry.ErrGeneration
	}
	var scope workerclient.Execution
	if err = json.Unmarshal([]byte(expected), &scope); err != nil {
		return p, err
	}
	var ack struct {
		Applied  int64  `json:"applied_revision"`
		Instance string `json:"instance_id"`
	}
	err = client.Lifecycle(ctx, "ApplyLifecyclePolicy", key, scope, map[string]any{"new_revision": p.DesiredRevision, "expected_applied_revision": p.AppliedRevision, "effective_policy": json.RawMessage(raw)}, &ack)
	if err != nil {
		_, _ = e.DB.DB().ExecContext(ctx, "UPDATE machine_lifecycle_policies SET last_error=? WHERE machine_id=? AND desired_revision=?", err.Error(), b.MachineID, p.DesiredRevision)
		return p, err
	}
	if ack.Applied != p.DesiredRevision || ack.Instance != scope.InstanceID {
		return p, registry.ErrGeneration
	}
	tx, err := e.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE machine_lifecycle_policies SET applied_revision=?,applied_json=?,last_error=NULL WHERE machine_id=? AND desired_revision=?
        AND EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=?)`, ack.Applied, raw, b.MachineID, ack.Applied, b.MachineID, b.ID)
	if err != nil {
		return p, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return p, err
	}
	if n != 1 {
		return p, registry.ErrRevision
	}
	if _, err = tx.ExecContext(ctx, `UPDATE machines SET keep_running=?,idle_policy_json=json_object('enabled',json(?),'timeout_seconds',?) WHERE id=?`,
		p.Desired.KeepRunning, fmt.Sprint(p.Desired.AutoHibernate), p.Desired.HibernateAfter, b.MachineID); err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_policy_application_targets SET state='applied' WHERE state='pending' AND machine_id=? AND binding_id=? AND policy_revision=?", b.MachineID, b.ID, ack.Applied); err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_deliveries SET delivered_at=? WHERE id=?", time.Now().UnixMilli(), key); err != nil {
		return p, err
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	return e.DB.LifecyclePolicy(ctx, b.MachineID)
}
func (e *Engine) ObservePolicy(ctx context.Context, b registry.Binding, client workerclient.Client) (WorkerLifecycleStatus, error) {
	var st WorkerLifecycleStatus
	if _, err := e.ApplyMachinePolicy(ctx, b, client); err != nil {
		return st, err
	}
	var raw json.RawMessage
	if err := client.Lifecycle(ctx, "GetLifecycleStatus", "", WorkerExecution(b), map[string]any{}, &raw); err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Instance != b.RuntimeInstanceObjectID {
		return st, registry.ErrGeneration
	}
	var cursor int64
	err := e.DB.ReadDB().QueryRowContext(ctx, "SELECT seq FROM lifecycle_outbox_cursors WHERE machine_id=?", b.MachineID).Scan(&cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	type event struct {
		Seq    int64   `json:"seq"`
		ID     string  `json:"operation_id"`
		Kind   string  `json:"kind"`
		Origin string  `json:"origin"`
		Stage  string  `json:"stage"`
		At     int64   `json:"occurred_at_ms"`
		Error  *string `json:"error"`
	}
	var page struct {
		Events []event `json:"events"`
	}
	if err = client.Lifecycle(ctx, "QueryLifecycleEvents", "", WorkerExecution(b), map[string]int64{"after": cursor}, &page); err != nil {
		return st, err
	}
	tx, err := e.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE machine_lifecycle_policies SET worker_status=? WHERE machine_id=? AND applied_revision=?", string(raw), b.MachineID, st.Applied); err != nil {
		return st, err
	}
	var latest int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce((SELECT seq FROM lifecycle_outbox_cursors WHERE machine_id=?),0)", b.MachineID).Scan(&latest); err != nil {
		return st, err
	}
	for _, event := range page.Events {
		if event.Seq <= latest {
			continue
		}
		resource := registry.ResourceEvent{Kind: "machine", ID: b.MachineID, Name: "lifecycle." + event.Kind + "." + event.Stage, Audience: "read"}
		if err = registry.AppendResourceEvent(ctx, tx, "machine", b.MachineID, "", resource, JSON(event), false); err != nil {
			return st, err
		}
		latest = event.Seq
	}
	if latest > cursor {
		if _, err = tx.ExecContext(ctx, "INSERT INTO lifecycle_outbox_cursors(machine_id,seq)VALUES(?,?) ON CONFLICT(machine_id) DO UPDATE SET seq=max(seq,excluded.seq)", b.MachineID, latest); err != nil {
			return st, err
		}
	}
	return st, tx.Commit()
}
