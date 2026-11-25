package lifecycle

import (
	"context"
	"errors"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	"time"
)

// Access registers demand before observing the VM. The durable worker owns any
// restore beyond the life of the first connection.
func (e *Engine) Access(ctx context.Context, p auth.Principal, id string) (registry.Binding, func(), error) {
	var zero registry.Binding
	ctl, err := e.DB.Control(ctx)
	if err != nil {
		return zero, nil, err
	}
	if ctl.Maintenance {
		return zero, nil, registry.ErrMaintenance
	}
	e.mu.Lock()
	if e.holds[id] >= e.Config.Gateway.MaxWaiters {
		e.mu.Unlock()
		return zero, nil, &Failure{"RESOURCE_EXHAUSTED", "too many concurrent access requests"}
	}
	e.holds[id]++
	e.mu.Unlock()
	release := func() {
		e.mu.Lock()
		if e.holds[id] <= 1 {
			delete(e.holds, id)
		} else {
			e.holds[id]--
		}
		e.mu.Unlock()
	}
	b, err := e.waitForRunning(ctx, p, ctl, id)
	if err != nil {
		release()
		return zero, nil, err
	}
	_, err = e.DB.DB().ExecContext(ctx, "UPDATE machines SET last_user_activity_at=? WHERE id=? AND current_binding_id=?", time.Now().UnixMilli(), id, b.ID)
	if err != nil {
		release()
		return zero, nil, err
	}
	return b, release, nil
}
func (e *Engine) waitForRunning(parent context.Context, p auth.Principal, ctl registry.Control, id string) (registry.Binding, error) {
	var zero registry.Binding
	ctx, cancel := context.WithTimeout(parent, time.Duration(e.Config.Gateway.WaitSeconds)*time.Second)
	defer cancel()
	key, err := registry.NewID("access_")
	if err != nil {
		return zero, err
	}
	for {
		m, err := e.DB.GetMachine(ctx, id)
		if err != nil {
			return zero, err
		}
		b := m.CurrentBinding
		if b == nil || m.DeletedAt != nil {
			return zero, registry.ErrNotFound
		}
		if m.ActiveOperationID != "" {
			op, err := e.DB.GetOperation(ctx, m.ActiveOperationID)
			if err != nil {
				return zero, err
			}
			if op.Kind == "idle_pause" {
				if e := e.rememberWake(ctx, op); e != nil {
					return zero, e
				}
			}
			switch op.Kind {
			case "wake", "resume", "idle_pause", "start", "sync_keys":
				if op.State == registry.RecoveryRequired {
					return zero, &Failure{"RECOVERY_REQUIRED", "pending machine operation requires observation"}
				}
			default:
				return zero, &registry.ActiveOperationError{MachineID: id, OperationID: op.ID}
			}
		} else {
			o, err := e.Runtime.Observe(ctx, b.RuntimeName)
			if err != nil {
				return zero, err
			}
			if o.Labels["platform.binding"] != b.ID {
				return zero, registry.ErrStaleObservation
			}
			switch o.State {
			case "running":
				if !o.Identity.Complete() {
					return zero, &Failure{"RECOVERY_REQUIRED", "runtime identity is incomplete"}
				}
				// Mutations accepted after this point see the hold at preflight.
				latest, err := e.DB.GetMachine(ctx, id)
				if err != nil {
					return zero, err
				}
				if latest.ActiveOperationID == "" && latest.CurrentBinding != nil && latest.CurrentBinding.ID == b.ID {
					return *b, nil
				}
			case "paused", "stopped", "created":
				if o.State != "paused" && !e.Config.Lifecycle.WakeStopped {
					return zero, &Failure{"SERVICE_NOT_READY", "cold wake is disabled"}
				}
				op, _, err := e.DB.Accept(ctx, registry.AcceptRequest{PrincipalID: p.ID, CredentialID: p.CredentialID, Epoch: ctl.ControlEpoch, Kind: "wake", Scope: "POST /internal/access/" + id, Key: key, MachineID: id, ExpectedGeneration: b.Generation, Heavy: true, Request: JSON(map[string]string{"reason": "user_access"})})
				if err != nil && !errors.Is(err, registry.ErrOperationActive) {
					return zero, err
				}
				if op.Terminal() || op.State == registry.RecoveryRequired {
					return zero, &Failure{"SERVICE_NOT_READY", "wake ended without a usable execution"}
				}
			default:
				return zero, &Failure{"RECOVERY_REQUIRED", "current state cannot be woken"}
			}
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(80 * time.Millisecond):
		}
	}
}

// rememberWake persists demand once native idle capture may be in progress.
// The successor is scheduled even if the originating HTTP connection closes.
func (e *Engine) rememberWake(ctx context.Context, op registry.Operation) error {
	if op.WakePending {
		return nil
	}
	tx, err := e.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM operations WHERE id=?", op.ID).Scan(&state); err != nil {
		return err
	}
	if state == registry.Queued {
		_, err = e.DB.TransitionTx(ctx, tx, op.ID, registry.TransitionRequest{State: registry.Cancelled, Stage: "completed", Effect: registry.EffectNone})
	} else if state == registry.Dispatching || state == registry.Observing || state == registry.Succeeded {
		_, err = tx.ExecContext(ctx, "UPDATE operations SET wake_pending=1 WHERE id=?", op.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (e *Engine) wakeSuccessors(ctx context.Context) {
	rows, err := e.DB.ReadDB().QueryContext(ctx, `SELECT o.id,o.machine_id,o.binding_generation,o.control_epoch,o.principal_id FROM operations o JOIN machines m ON m.id=o.machine_id JOIN meta ON meta.id=1 WHERE o.kind='idle_pause' AND o.wake_pending AND o.state='succeeded' AND o.control_epoch=meta.control_epoch AND meta.maintenance=0 AND m.deleted_at IS NULL AND m.binding_generation=o.binding_generation`)
	if err != nil {
		return
	}
	var pending []registry.AcceptRequest
	for rows.Next() {
		var r registry.AcceptRequest
		var id string
		if err = rows.Scan(&id, &r.MachineID, &r.ExpectedGeneration, &r.Epoch, &r.PrincipalID); err != nil {
			break
		}
		r.Kind = "wake"
		r.Heavy = true
		r.Key = id
		r.Scope = "POST /internal/wake-pending/" + r.MachineID
		r.Request = JSON(map[string]string{"reason": "activity_during_idle", "pause_operation_id": id})
		pending = append(pending, r)
	}
	rows.Close()
	for _, r := range pending {
		m, err := e.DB.GetMachine(ctx, r.MachineID)
		if err != nil {
			continue
		}
		var newer int
		err = e.DB.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE machine_id=? AND created_at>(SELECT created_at FROM operations WHERE id=?) AND kind IN ('pause','stop','delete','rollback','start','resume','wake')`, r.MachineID, r.Key).Scan(&newer)
		if err != nil {
			continue
		}
		if newer == 0 && m.ObservedState == "paused" {
			_, _, err = e.DB.Accept(ctx, r)
			if err != nil {
				continue
			}
		} else if newer == 0 {
			continue
		}
		_, _ = e.DB.DB().ExecContext(ctx, "UPDATE operations SET wake_pending=0 WHERE id=?", r.Key)
	}
}
