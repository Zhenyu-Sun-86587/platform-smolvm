package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

type WorkerMaintenanceObservations struct {
	CollectorsActive      bool
	Rules                 []registry.PacketWakeRuleEvidence
	PendingWakeOperations []string
	UnresolvedResources   []string
}

// ReconcileWorkerMaintenance leaves activity collection running and applies
// the overlay to known bindings. In Drain it disables each registered rule
// with the current rule revision and cancels only known pending wake demands.
func (e *Engine) ReconcileWorkerMaintenance(ctx context.Context, worker string, revision int64) (WorkerMaintenanceObservations, error) {
	var observed WorkerMaintenanceObservations
	state, err := e.DB.GetWorkerMaintenance(ctx, worker)
	if err != nil {
		return observed, err
	}
	if state.Revision != revision {
		return observed, registry.ErrRevision
	}
	if state.Mode != "draining" && state.Phase != "activating" {
		return observed, nil
	}
	machines, err := e.DB.ListMachines(ctx)
	if err != nil {
		return observed, err
	}
	observed.CollectorsActive = true
	for _, machine := range machines {
		b := machine.CurrentBinding
		if b == nil || b.WorkerID != worker {
			continue
		}
		if b.RuntimeInstanceObjectID == "" {
			observed.UnresolvedResources = append(observed.UnresolvedResources, b.ID)
			observed.CollectorsActive = false
			continue
		}
		adapter, err := e.ForBinding(*b)
		client, ok := PolicyClient(adapter, *b)
		if err != nil || !ok {
			observed.UnresolvedResources = append(observed.UnresolvedResources, b.ID)
			observed.CollectorsActive = false
			continue
		}
		status, err := e.ObservePolicy(ctx, *b, client)
		if err != nil {
			observed.UnresolvedResources = append(observed.UnresolvedResources, b.ID)
			observed.CollectorsActive = false
			continue
		}
		if len(status.Collectors) == 0 {
			observed.CollectorsActive = false
		}
		if decision := status.Decision; decision != nil && decision.Origin == "wake" &&
			slices.Contains([]string{"queued", "dispatching", "running"}, decision.Stage) {
			observed.PendingWakeOperations = append(observed.PendingWakeOperations, decision.ID)
		}
	}
	if state.Mode != "draining" {
		return observed, nil
	}
	rules, err := e.DB.ListWorkerPacketWakeRules(ctx, worker)
	if err != nil {
		return observed, err
	}
	for _, rule := range rules {
		proof, err := e.disableManagedPacketWakeRule(ctx, rule, revision)
		if err != nil {
			observed.UnresolvedResources = append(observed.UnresolvedResources,
				fmt.Sprintf("%s:%s:%d", rule.SourceAttachmentID, rule.Transport, rule.Port))
			continue
		}
		observed.Rules = append(observed.Rules, proof)
	}
	if err := e.cancelManagedPendingWakes(ctx, worker, revision, &observed); err != nil {
		return observed, err
	}
	return observed, nil
}

func (e *Engine) disableManagedPacketWakeRule(ctx context.Context, rule registry.WorkerPacketWakeRule, maintenanceRevision int64) (registry.PacketWakeRuleEvidence, error) {
	var zero registry.PacketWakeRuleEvidence
	b, err := e.DB.GetBinding(ctx, rule.BindingID)
	if err != nil {
		return zero, err
	}
	if b.WorkerID != rule.WorkerID || b.Status != "current" {
		return zero, registry.ErrGeneration
	}
	adapter, err := e.ForBinding(b)
	if err != nil {
		return zero, err
	}
	client, ok := PolicyClient(adapter, b)
	if !ok {
		return zero, registry.ErrNotFound
	}
	scope := WorkerExecution(b)
	req := workerclient.PacketWakeRule{SourceAttachmentID: rule.SourceAttachmentID, TargetAttachmentID: rule.TargetAttachmentID,
		Transport: rule.Transport, GuestPort: rule.Port}
	type ruleReply struct {
		ID        string `json:"rule_id"`
		Revision  int64  `json:"revision"`
		Enabled   *bool  `json:"enabled"`
		Source    string `json:"source_attachment_id"`
		Target    string `json:"target_attachment_id"`
		Transport string `json:"transport"`
		Port      uint16 `json:"guest_port"`
	}
	parse := func(raw json.RawMessage) (ruleReply, error) {
		var reply ruleReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return reply, err
		}
		if reply.ID == "" || reply.Revision < 1 || reply.Enabled == nil || reply.Source != req.SourceAttachmentID ||
			reply.Target != req.TargetAttachmentID || reply.Transport != req.Transport || reply.Port != req.GuestPort {
			return reply, registry.ErrGeneration
		}
		return reply, nil
	}
	raw, err := client.PacketWakeRule(ctx, scope, req, "", "")
	if err != nil {
		return zero, err
	}
	before, err := parse(raw)
	if err != nil {
		return zero, err
	}
	if !*before.Enabled {
		return registry.PacketWakeRuleEvidence{ID: before.ID, Revision: before.Revision}, nil
	}
	state, err := e.DB.GetWorkerMaintenance(ctx, rule.WorkerID)
	if err != nil {
		return zero, err
	}
	if state.Mode != "draining" || state.Revision != maintenanceRevision {
		return zero, registry.ErrRevision
	}
	enabled := false
	req.Enabled, req.ExpectedRevision = &enabled, &before.Revision
	key := fmt.Sprintf("maintenance-packet-%x", sha256.Sum256(JSON([]any{before.ID, b.ID, scope, maintenanceRevision, before.Revision})))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(req)))
	raw, err = client.PacketWakeRule(ctx, scope, req, key, digest)
	if err != nil {
		return zero, err
	}
	after, err := parse(raw)
	if err != nil {
		return zero, err
	}
	if after.ID != before.ID || after.Revision != before.Revision+1 || *after.Enabled {
		return zero, registry.ErrRevision
	}
	return registry.PacketWakeRuleEvidence{ID: after.ID, Revision: after.Revision}, nil
}

func (e *Engine) cancelManagedPendingWakes(ctx context.Context, worker string, revision int64, observed *WorkerMaintenanceObservations) error {
	rows, err := e.DB.ReadDB().QueryContext(ctx, `SELECT a.id,a.binding_id,a.principal,a.execution_json FROM connection_activity a
		JOIN runtime_bindings b ON b.id=a.binding_id WHERE b.worker_id=? ORDER BY a.id`, worker)
	if err != nil {
		return err
	}
	var connections []connectionActivity
	for rows.Next() {
		var c connectionActivity
		var scope string
		if err := rows.Scan(&c.id, &c.binding, &c.principal, &scope); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(scope), &c.execution); err != nil {
			rows.Close()
			return err
		}
		connections = append(connections, c)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range connections {
		b, err := e.DB.GetBinding(ctx, c.binding)
		if err != nil || b.WorkerID != worker || b.Status != "current" {
			observed.UnresolvedResources = append(observed.UnresolvedResources, c.id)
			continue
		}
		adapter, err := e.ForBinding(b)
		client, ok := PolicyClient(adapter, b)
		if err != nil || !ok {
			observed.UnresolvedResources = append(observed.UnresolvedResources, c.id)
			continue
		}
		args := map[string]any{"protection_id": c.id, "principal": c.principal}
		var ack protectionReceipt
		if err := client.Lifecycle(ctx, "QueryProtection", "", c.execution, args, &ack); err != nil {
			var fault *workerclient.Fault
			if !errors.As(err, &fault) || fault.Code != "E_NOT_FOUND" {
				observed.UnresolvedResources = append(observed.UnresolvedResources, c.id)
			}
			continue
		}
		if ack.Kind != "wake" || ack.State != "pending" || ack.ID != c.id || ack.Revision <= 0 {
			continue
		}
		if ack.Wake != "" {
			observed.PendingWakeOperations = append(observed.PendingWakeOperations, ack.Wake)
			if err := e.DB.ObserveAccessWakeDecision(ctx, c.id, ack.Wake); err != nil {
				return err
			}
		}
		state, err := e.DB.GetWorkerMaintenance(ctx, worker)
		if err != nil || state.Mode != "draining" || state.Revision != revision {
			if err != nil {
				return err
			}
			return registry.ErrRevision
		}
		args["expected_revision"] = ack.Revision
		key := fmt.Sprintf("%s-maintenance-%d-%d", c.id, revision, ack.Revision)
		var cancelled protectionReceipt
		if err := client.Lifecycle(ctx, "CancelWakeDemand", key, WorkerExecution(b), args, &cancelled); err != nil ||
			cancelled.ID != c.id || cancelled.State != "released" {
			observed.UnresolvedResources = append(observed.UnresolvedResources, c.id)
		}
	}
	return nil
}
