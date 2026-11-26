package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

type connectionActivity struct {
	id, binding, principal string
	execution              workerclient.Execution
}

// The registry holds an exclusive process lock. A connection from an older
// collector session cannot still be forwarding on behalf of this installation.
func (e *Engine) registerConnection(ctx context.Context, b registry.Binding, id, principal string, scope workerclient.Execution) (connectionActivity, error) {
	connection := connectionActivity{id: id, binding: b.ID, principal: principal, execution: scope}
	session := e.activitySession()
	if session == "" {
		return connection, errors.New("unable to identify platform activity session")
	}
	_, err := e.DB.DB().ExecContext(ctx, "INSERT INTO connection_activity(id,binding_id,principal,execution_json,owner_session) VALUES(?,?,?,?,?)",
		id, b.ID, principal, string(JSON(scope)), session)
	return connection, err
}

func (a connectionActivity) end(ctx context.Context, client workerclient.LifecycleControl) error {
	args := map[string]any{"protection_id": a.id, "principal": a.principal}
	var ack protectionReceipt
	err := client.Lifecycle(ctx, "CloseAccessProtection", a.id+"-end", a.execution, args, &ack)
	var fault *workerclient.Fault
	if errors.As(err, &fault) && fault.Code == "E_CAPABILITY_UNSUPPORTED" {
		// A historical Worker supports revision-checked release. An unknown
		// receipt stays in the journal until the original request is resolved.
		if err = client.Lifecycle(ctx, "QueryProtection", "", a.execution, args, &ack); err != nil {
			return err
		}
		if ack.State != "released" {
			method := "ReleaseActivityHold"
			if ack.Kind == "wake" {
				method = "CancelWakeDemand"
			} else if ack.Kind != "hold" {
				return registry.ErrConflict
			}
			var current WorkerLifecycleStatus
			if err = client.Lifecycle(ctx, "GetLifecycleStatus", "", a.execution, map[string]any{}, &current); err != nil {
				return err
			}
			if current.Instance != a.execution.InstanceID {
				return registry.ErrGeneration
			}
			args["expected_revision"] = ack.Revision
			err = client.Lifecycle(ctx, method, fmt.Sprintf("%s-end-%d", a.id, ack.Revision), current.Execution(), args, &ack)
		}
	}
	if err == nil && (ack.ID != a.id || ack.State != "released") {
		return registry.ErrConflict
	}
	return err
}

func (e *Engine) closeConnection(ctx context.Context, a connectionActivity, client workerclient.LifecycleControl) {
	_, err := e.DB.DB().ExecContext(ctx, "UPDATE connection_activity SET closing=1,last_attempt=? WHERE id=?", time.Now().UnixMilli(), a.id)
	if err == nil && a.end(ctx, client) == nil {
		_, _ = e.DB.DB().ExecContext(ctx, "DELETE FROM connection_activity WHERE id=? AND closing=1", a.id)
	}
}

func (e *Engine) connectionRelease(a connectionActivity, client workerclient.LifecycleControl) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		e.closeConnection(ctx, a, client)
	}
}

// HoldConnectionActivity protects work forwarded over a live, authenticated
// platform connection. Explicit running leases have their own owner and TTL.
func (e *Engine) HoldConnectionActivity(ctx context.Context, b registry.Binding, client workerclient.LifecycleControl, principal, reason string) (func(), error) {
	id, err := registry.NewID("access-")
	if err != nil {
		return nil, err
	}
	a, err := e.registerConnection(ctx, b, id, principal, WorkerExecution(b))
	if err != nil {
		return nil, err
	}
	release := e.connectionRelease(a, client)
	args := map[string]any{"protection_id": id, "principal": principal, "reason": reason}
	if err = client.Lifecycle(ctx, "AcquireActivityHold", id+"-open", a.execution, args, nil); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (e *Engine) recoverConnections(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		e.retryClosedConnections(ctx)
		if e.DB.ExpireDesktopLeases(ctx, time.Now()) == nil && e.ReleaseDesktopLeases != nil {
			e.ReleaseDesktopLeases(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) retryClosedConnections(ctx context.Context) {
	session := e.activitySession()
	if session == "" {
		return
	}
	rows, err := e.DB.ReadDB().QueryContext(ctx, "SELECT id,binding_id,principal,execution_json FROM connection_activity WHERE owner_session<>? OR closing=1 ORDER BY last_attempt,id LIMIT 32", session)
	if err != nil {
		return
	}
	var pending []connectionActivity
	for rows.Next() {
		var a connectionActivity
		var scope string
		if err = rows.Scan(&a.id, &a.binding, &a.principal, &scope); err != nil {
			break
		}
		if json.Unmarshal([]byte(scope), &a.execution) == nil {
			pending = append(pending, a)
		}
	}
	rows.Close()
	for _, a := range pending {
		if ctx.Err() != nil {
			return
		}
		b, err := e.DB.GetBinding(ctx, a.binding)
		if err != nil || b.RuntimeInstanceObjectID != a.execution.InstanceID {
			continue
		}
		adapter, err := e.ForBinding(b)
		if err != nil {
			continue
		}
		if client, ok := PolicyClient(adapter, b); ok {
			e.closeConnection(ctx, a, client)
		}
	}
}
