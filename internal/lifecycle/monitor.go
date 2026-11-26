package lifecycle

import (
	"context"
	"encoding/json"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/tasks"
	"strings"
	"time"
)

// monitor samples public execution state without extending user idle time.
func (e *Engine) monitor(ctx context.Context) {
	started := time.Now()
	bootID := hostBootID()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		e.sample(ctx, started, bootID)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (e *Engine) sample(ctx context.Context, started time.Time, bootID string) {
	ctl, err := e.DB.Control(ctx)
	if err != nil || ctl.Maintenance {
		return
	}
	officialReady := readyRuntime(ctx, e.OfficialRuntime()) == nil
	ms, err := e.DB.ListMachines(ctx)
	if err != nil {
		return
	}
	if officialReady {
		_, _ = e.DB.DB().ExecContext(ctx, "UPDATE leases SET state='suspect',revision=revision+1 WHERE state='active' AND worker_revision IS NULL AND expires_at<=?", time.Now().UnixMilli())
	}
	for _, m := range ms {
		if m.CurrentBinding == nil || m.ActiveOperationID != "" || ctx.Err() != nil {
			continue
		}
		b := *m.CurrentBinding
		custom := !bindingRuntime(b).Legacy()
		if !custom && !officialReady {
			continue
		}
		var expectedEpoch *int64
		if custom {
			expectedEpoch = &b.ExecutionEpoch
		}
		adapter, err := e.ForBinding(b)
		if err != nil {
			continue
		}
		if custom && b.RuntimeInstanceObjectID != "" {
			if client, ok := PolicyClient(adapter, b); ok {
				if e.ReconcileWorkerLeases != nil {
					e.ReconcileWorkerLeases(ctx, b, client)
				}
				if status, err := e.ObservePolicy(ctx, b, client); err == nil {
					if status.Power == "STOPPED" && b.PowerState == registry.PowerStopped && status.Execution() == WorkerExecution(b) {
						_ = e.DB.ObserveMachine(ctx, b.ID, registry.Observation{ExpectedExecutionEpoch: &b.ExecutionEpoch})
						continue
					}
					if status.Decision != nil && status.Decision.Stage == "completed" && (status.Power == "HIBERNATED" || status.Epoch > b.ExecutionEpoch) {
						_ = e.projectAutoPower(ctx, b, adapter, status, status.Decision.ID)
					}
					e.reportActivity(ctx, m, b, adapter, client, status)
				}
			}
			// Public machine metadata cannot identify a native execution epoch.
			e.observeNativePower(ctx, b, adapter)
			continue
		}
		o, err := adapter.Observe(ctx, b.RuntimeName)
		if err != nil {
			_ = e.DB.ObserveMachine(ctx, b.ID, registry.Observation{ObservedState: "unknown", Availability: "blocked", GuestManagementStatus: "unknown", ExpectedExecutionEpoch: expectedEpoch})
			continue
		}
		if o.Labels["platform.binding"] != b.ID {
			_ = e.DB.ObserveMachine(ctx, b.ID, registry.Observation{ObservedState: "unknown", Availability: "blocked", ExpectedExecutionEpoch: expectedEpoch})
			continue
		}
		if custom {
			if e.observeNativePower(ctx, b, adapter) {
				continue
			}
			x := Evidence(o)
			x.ExpectedRuntimeIdentity = b.RuntimeIdentity
			x.ExpectedExecutionEpoch = expectedEpoch
			x.Availability = "blocked"
			x.GuestManagementStatus = "unknown"
			if o.Identity.Complete() {
				x.WorkloadError = o.WorkloadError
			}
			_ = e.DB.ObserveMachine(ctx, b.ID, x)
			continue
		}
		var uncertainty string
		switch {
		case stoppedAfterHostReboot(bootID, b, o):
			uncertainty = "host_boot_changed"
		case o.PressurePaused():
			uncertainty = "native_pressure_pause"
		case o.State == "failed" || (o.State == "stopped" && b.ObservedState == "running"):
			uncertainty = "native_execution_ended"
		}
		if uncertainty != "" {
			if err := e.recordTaskUncertainty(ctx, b, o.ObservedAt, uncertainty); err != nil {
				continue
			}
		}
		x := Evidence(o)
		x.ExpectedRuntimeIdentity = b.RuntimeIdentity
		tpl, err := e.DB.GetTemplate(ctx, m.TemplateID)
		managed := err == nil && tpl.GuestRelease != ""
		idleAllowed := false
		if o.State == "running" && managed {
			res, err := e.Call(ctx, b, tasks.Request{Method: "hello"})
			if err != nil || res.Binding == nil || *res.Binding != *GuestBinding(b) || res.Hello == nil {
				x.GuestManagementStatus = "degraded"
			} else {
				x.GuestManagementStatus = "ready"
				x.GuestManagementEvidence = JSON(res.Hello)
				idleAllowed = res.Hello.Cgroup && res.Hello.Active == 0 && res.Hello.Unknown == 0 && res.Hello.Barrier == nil
				if listed, err := e.Call(ctx, b, tasks.Request{Method: "list"}); err == nil {
					for _, t := range listed.Tasks {
						if t.MachineID != m.ID || t.Namespace != b.TaskNamespace {
							idleAllowed = false
							continue
						}
						_, err = e.DB.DB().ExecContext(ctx, `UPDATE tasks SET state=?,exit_code=?,observed_at=?,result_json=? WHERE id=? AND task_namespace=? AND observed_at<=? AND EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=? AND deleted_at IS NULL)`, t.State, t.ExitCode, t.ObservedAt.UnixMilli(), string(JSON(t)), t.ID, t.Namespace, t.ObservedAt.UnixMilli(), m.ID, b.ID)
						if err != nil || !t.Terminal() {
							idleAllowed = false
						}
					}
				} else {
					idleAllowed = false
				}
			}
		}
		if x.GuestManagementStatus == "ready" {
			e.refreshSSH(ctx, m, &x)
		}
		if err = e.DB.ObserveMachine(ctx, b.ID, x); err != nil {
			continue
		}
		if time.Since(started) < time.Duration(e.Config.Lifecycle.StartupGuard)*time.Second || !idleAllowed || m.KeepRunning || m.LastUserActivityAt == nil || strings.Contains(strings.ToLower(o.StateReason), "pressure") {
			continue
		}
		var policy IdlePolicy
		if json.Unmarshal(m.IdlePolicy, &policy) != nil || !policy.Enabled || time.Since(*m.LastUserActivityAt) < time.Duration(policy.Timeout)*time.Second {
			continue
		}
		var n int
		if err = e.DB.ReadDB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM leases WHERE machine_id=? AND state!='released' AND binding_generation=?) + (SELECT count(*) FROM dependencies WHERE parent_binding_id=? AND kind='live_branch_resource' AND status!='released') + (SELECT count(*) FROM exec_sessions WHERE machine_id=? AND state NOT IN ('done','exited','lost','failed'))`, m.ID, b.Generation, b.ID, m.ID).Scan(&n); err != nil || n > 0 {
			continue
		}
		var owner string
		if owner, err = e.DB.OwnerID(ctx); err != nil {
			continue
		}
		ctl, err := e.DB.Control(ctx)
		if err != nil {
			continue
		}
		e.mu.Lock()
		if e.holds[m.ID] == 0 {
			key, _ := registry.NewID("idle_")
			_, _, _ = e.DB.Accept(ctx, registry.AcceptRequest{PrincipalID: owner, Epoch: ctl.ControlEpoch, Key: key, Scope: "POST /internal/idle/" + m.ID, Kind: "idle_pause", MachineID: m.ID, ExpectedRevision: m.Revision, ExpectedGeneration: b.Generation, Heavy: true, Request: JSON(map[string]any{"reason": "idle", "last_user_activity_at": m.LastUserActivityAt})})
		}
		e.mu.Unlock()
	}
}
