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
	Observation          json.RawMessage `json:"-"`
	FirstLaunchSupported bool            `json:"first_launch_supported"`
	StatePoint           string          `json:"statepoint"`
	CollectorGeneration  string          `json:"collector_generation"`
	Instance             string          `json:"instance_id"`
	Incarnation          string          `json:"incarnation_id"`
	Epoch                int64           `json:"execution_epoch"`
	Revision             int64           `json:"binding_revision"`
	Applied              int64           `json:"applied_revision"`
	Power                string          `json:"power"`
	Suspended            bool            `json:"controlled_suspended"`
	Blockers             []string        `json:"blockers"`
	Collectors           map[string]struct {
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

func PolicyClient(adapter rt.Adapter, b registry.Binding) (workerclient.LifecycleControl, bool) {
	if b.RuntimeInstanceObjectID == "" || bindingRuntime(b).Legacy() {
		return nil, false
	}
	if remote, ok := adapter.(workerclient.LifecycleControl); ok {
		return remote, true
	}
	p, ok := adapter.(interface{ LifecycleWorkerSocket() string })
	if !ok || p.LifecycleWorkerSocket() == "" {
		return nil, false
	}
	return workerclient.Client{Socket: p.LifecycleWorkerSocket(), Bundle: b.RuntimeBundleID}, true
}
func WorkerExecution(b registry.Binding) workerclient.Execution {
	incarnation := b.IncarnationID
	if b.PowerState == registry.PowerStopped {
		// The registry keeps the last incarnation for history. A stopped
		// Worker slot has no live incarnation at that same epoch.
		incarnation = ""
	}
	return workerclient.Execution{InstanceID: b.RuntimeInstanceObjectID, IncarnationID: incarnation, Epoch: b.ExecutionEpoch}
}
func (s WorkerLifecycleStatus) Execution() workerclient.Execution {
	return workerclient.Execution{InstanceID: s.Instance, IncarnationID: s.Incarnation, Epoch: s.Epoch}
}

type policyWorker interface {
	Lifecycle(context.Context, string, string, workerclient.Execution, any, any) error
}

type policyReceipt struct {
	Applied   int64               `json:"applied_revision"`
	Instance  string              `json:"instance_id"`
	Effective *rt.LifecyclePolicy `json:"effective_policy"`
}

func deliverPolicy(ctx context.Context, client policyWorker, key string, scope workerclient.Execution, base, desired int64, raw json.RawMessage) error {
	var policy rt.LifecyclePolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return err
	}
	apply := func(id string, prior, revision int64, initialize bool) error {
		request := map[string]any{"new_revision": revision, "expected_applied_revision": prior, "effective_policy": raw}
		if initialize {
			request["initialize_replacement"] = true
		}
		var ack policyReceipt
		if err := client.Lifecycle(ctx, "ApplyLifecyclePolicy", id, scope, request, &ack); err != nil {
			return err
		}
		if ack.Applied != revision || ack.Instance != scope.InstanceID || ack.Effective != nil && *ack.Effective != policy {
			return registry.ErrGeneration
		}
		return nil
	}
	initialize := base == 0 && desired > 1
	err := apply(key, base, desired, initialize)
	var fault *workerclient.Fault
	if base != 0 || !errors.As(err, &fault) || !(fault.Code == "E_STALE_REVISION" || initialize && fault.Code == "E_INVALID_REQUEST") {
		return err
	}

	// The binding can also be promoted again on an existing physical instance.
	// Its own receipt establishes the revision from which it is safe to continue.
	var current policyReceipt
	if err := client.Lifecycle(ctx, "GetLifecycleStatus", "", scope, map[string]any{}, &current); err != nil {
		if !errors.As(err, &fault) || fault.Code != "E_NOT_FOUND" {
			return err
		}
	} else if current.Instance != scope.InstanceID || current.Applied <= 0 || current.Effective == nil {
		return registry.ErrGeneration
	}
	if current.Applied >= desired {
		if current.Applied == desired && *current.Effective == policy {
			return nil
		}
		return registry.ErrRevision
	}
	// Historical Workers only advance by one. Install the desired protection on
	// the first call and keep it in force while catching up the revision counter.
	for prior := current.Applied; prior < desired; prior++ {
		revision := prior + 1
		if err := apply(fmt.Sprintf("%s-sync-%d", key, revision), prior, revision, false); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ApplyMachinePolicy(ctx context.Context, b registry.Binding, client policyWorker) (registry.LifecycleSettings, error) {
	p, err := e.DB.LifecyclePolicy(ctx, b.MachineID)
	if err != nil {
		return p, err
	}
	if p.BindingID == "" || p.BindingID != b.ID {
		return p, registry.ErrGeneration
	}
	if p.AppliedRevision == p.DesiredRevision {
		return p, nil
	}
	key := fmt.Sprintf("binding-policy-%s-%d", b.ID, p.DesiredRevision)
	encoded := JSON(p.Desired)
	ref := JSON(WorkerExecution(b))
	_, err = e.DB.DB().ExecContext(ctx, `INSERT INTO lifecycle_deliveries(id,machine_id,binding_id,desired_revision,policy_json,expected_json,previous_revision)
		SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=? AND deleted_at IS NULL)
		ON CONFLICT(binding_id,desired_revision) DO NOTHING`, key, b.MachineID, b.ID, p.DesiredRevision, string(encoded), string(ref), p.AppliedRevision, b.MachineID, b.ID)
	if err != nil {
		return p, err
	}
	var raw, expected string
	var previous int64
	// An in-flight delivery created before the binding-scoped journal upgrade
	// keeps its original request ID and execution fingerprint across a retry.
	err = e.DB.ReadDB().QueryRowContext(ctx, "SELECT id,policy_json,expected_json,previous_revision FROM lifecycle_deliveries WHERE machine_id=? AND binding_id=? AND desired_revision=?",
		b.MachineID, b.ID, p.DesiredRevision).Scan(&key, &raw, &expected, &previous)
	if err != nil {
		return p, err
	}
	var scope workerclient.Execution
	if err = json.Unmarshal([]byte(expected), &scope); err != nil {
		return p, err
	}
	if err = deliverPolicy(ctx, client, key, scope, previous, p.DesiredRevision, json.RawMessage(raw)); err != nil {
		_, _ = e.DB.DB().ExecContext(ctx, `UPDATE machine_lifecycle_policies SET last_error=? WHERE machine_id=? AND desired_revision=? AND applied_revision<desired_revision
			AND EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=?)`, err.Error(), b.MachineID, p.DesiredRevision, b.MachineID, b.ID)
		return p, err
	}
	tx, err := e.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE machine_lifecycle_policies SET applied_revision=?,applied_json=?,last_error=NULL WHERE machine_id=? AND desired_revision=?
        AND EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=?)`, p.DesiredRevision, raw, b.MachineID, p.DesiredRevision, b.MachineID, b.ID)
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
	if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_policy_application_targets SET state='applied' WHERE state='pending' AND machine_id=? AND binding_id=? AND policy_revision=?", b.MachineID, b.ID, p.DesiredRevision); err != nil {
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
func (e *Engine) ObservePolicy(ctx context.Context, b registry.Binding, client policyWorker) (WorkerLifecycleStatus, error) {
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
	st.Observation = raw
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
	var binding string
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(current_binding_id,'') FROM machines WHERE id=?", b.MachineID).Scan(&binding); err != nil {
		return st, err
	}
	if binding != b.ID {
		return st, registry.ErrGeneration
	}
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
