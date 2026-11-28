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

type packetRuleReceipt struct {
	ID                string `json:"rule_id"`
	Revision          int64  `json:"revision"`
	Enabled           *bool  `json:"enabled"`
	Source            string `json:"source_attachment_id"`
	Target            string `json:"target_attachment_id"`
	Transport         string `json:"transport"`
	Port              uint16 `json:"guest_port"`
	SourceIncarnation string `json:"source_incarnation_id"`
	SourceEpoch       int64  `json:"source_execution_epoch"`
	SourceCurrent     bool   `json:"source_current"`
}

type packetRuleOverlay struct {
	ID        string
	Before    int64
	Key       string
	Target    workerclient.Execution
	Source    workerclient.Execution
	Confirmed bool
}

func checkedPacketRule(ctx context.Context, client workerclient.BindingControl, scope workerclient.Execution, req workerclient.PacketWakeRule, key string) (packetRuleReceipt, error) {
	digest := ""
	if key != "" {
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256(JSON(req)))
	}
	raw, err := client.PacketWakeRule(ctx, scope, req, key, digest)
	var reply packetRuleReceipt
	if err != nil {
		return reply, err
	}
	if err = json.Unmarshal(raw, &reply); err != nil {
		return reply, err
	}
	if reply.ID == "" || reply.Revision < 1 || reply.Enabled == nil || reply.Source != req.SourceAttachmentID || reply.Target != req.TargetAttachmentID ||
		reply.Transport != req.Transport || reply.Port != req.GuestPort {
		return reply, registry.ErrGeneration
	}
	return reply, nil
}

func (e *Engine) packetRuleControl(ctx context.Context, r registry.WorkerPacketWakeRule) (workerclient.BindingControl, workerclient.Execution, workerclient.PacketWakeRule, error) {
	var scope workerclient.Execution
	req := workerclient.PacketWakeRule{SourceAttachmentID: r.SourceAttachmentID, TargetAttachmentID: r.TargetAttachmentID, Transport: r.Transport, GuestPort: r.Port}
	b, err := e.DB.GetBinding(ctx, r.BindingID)
	if err != nil {
		return nil, scope, req, err
	}
	if b.WorkerID != r.WorkerID || b.Status != "current" {
		return nil, scope, req, registry.ErrGeneration
	}
	a, err := e.ForBinding(b)
	if err != nil {
		return nil, scope, req, err
	}
	c := workerclient.ForAdapter(a, b.RuntimeBundleID)
	if c == nil {
		return nil, scope, req, registry.ErrNotFound
	}
	return c, WorkerExecution(b), req, nil
}

func (e *Engine) disableManagedPacketWakeRule(ctx context.Context, r registry.WorkerPacketWakeRule, revision int64) (registry.PacketWakeRuleEvidence, error) {
	var proof registry.PacketWakeRuleEvidence
	client, scope, req, err := e.packetRuleControl(ctx, r)
	if err != nil {
		return proof, err
	}
	current, err := checkedPacketRule(ctx, client, scope, req, "")
	if err != nil {
		return proof, err
	}
	proof = registry.PacketWakeRuleEvidence{ID: current.ID, Revision: current.Revision, Enabled: *current.Enabled}
	var overlay packetRuleOverlay
	if r.Overlay != nil {
		if err = json.Unmarshal(r.Overlay, &overlay); err != nil {
			return proof, err
		}
	}
	// A crash after sending Disable must recover its own idempotent receipt.
	// An independent revision on the Worker is never a restoration authority.
	if !proof.Enabled && (overlay.ID == "" || overlay.Confirmed || current.Revision != overlay.Before+1) {
		return proof, nil
	}
	if proof.Enabled {
		overlay = packetRuleOverlay{ID: current.ID, Before: current.Revision, Target: scope,
			Key: fmt.Sprintf("maintenance-packet-%x", sha256.Sum256(JSON([]any{current.ID, r.BindingID, scope, revision, current.Revision})))}
		if r.DesiredEnabled && current.SourceCurrent && r.SourceBindingID != "" {
			src, err := e.DB.GetBinding(ctx, r.SourceBindingID)
			if err != nil {
				return proof, err
			}
			actual := WorkerExecution(src)
			if src.Status == "current" && src.WorkerID == r.WorkerID && actual.IncarnationID == current.SourceIncarnation && actual.Epoch == current.SourceEpoch {
				overlay.Source = actual
				if err = e.DB.SetPacketWakeOverlay(ctx, r, revision, overlay); err != nil {
					return proof, err
				}
			}
		}
	}
	state, err := e.DB.GetWorkerMaintenance(ctx, r.WorkerID)
	if err != nil {
		return proof, err
	}
	if state.Mode != "draining" || state.Revision != revision {
		return proof, registry.ErrRevision
	}
	disabled := false
	req.Enabled, req.ExpectedRevision = &disabled, &overlay.Before
	ack, err := checkedPacketRule(ctx, client, overlay.Target, req, overlay.Key)
	if err != nil {
		return proof, err
	}
	if ack.ID != overlay.ID || ack.Revision != overlay.Before+1 || *ack.Enabled {
		return proof, registry.ErrRevision
	}
	proof.Revision, proof.Enabled = ack.Revision, false
	if overlay.Source.InstanceID != "" {
		overlay.Confirmed = true
		err = e.DB.SetPacketWakeOverlay(ctx, r, revision, overlay)
	}
	return proof, err
}

// Reopen only the exact source execution and disabled revision that the
// maintenance cycle closed. The central gate stays closed during activation.
func (e *Engine) RestoreWorkerPacketRules(ctx context.Context, worker string, revision int64) ([]string, error) {
	rules, err := e.DB.ListWorkerPacketWakeRules(ctx, worker)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, r := range rules {
		if r.Overlay == nil || !r.DesiredEnabled {
			continue
		}
		var saved packetRuleOverlay
		if err = json.Unmarshal(r.Overlay, &saved); err != nil {
			return nil, err
		}
		if err := e.restorePacketRule(ctx, r, saved, revision); err != nil {
			pending = append(pending, saved.ID)
		}
	}
	return pending, nil
}

func (e *Engine) restorePacketRule(ctx context.Context, r registry.WorkerPacketWakeRule, saved packetRuleOverlay, revision int64) error {
	if !saved.Confirmed || saved.Source.InstanceID == "" {
		return registry.ErrStaleObservation
	}
	client, scope, req, err := e.packetRuleControl(ctx, r)
	if err != nil {
		return err
	}
	current, err := checkedPacketRule(ctx, client, scope, req, "")
	if err != nil {
		return err
	}
	if current.ID != saved.ID {
		return registry.ErrGeneration
	}
	if current.Revision > saved.Before+1 {
		// A completed restore or a newer local edit supersedes the overlay.
		return e.DB.SetPacketWakeOverlay(ctx, r, revision, nil)
	}
	if current.Revision != saved.Before+1 || *current.Enabled {
		return registry.ErrRevision
	}
	src, err := e.DB.GetBinding(ctx, r.SourceBindingID)
	if err != nil {
		return err
	}
	if src.Status != "current" || WorkerExecution(src) != saved.Source || src.WorkerID != r.WorkerID {
		return registry.ErrGeneration
	}
	state, err := e.DB.GetWorkerMaintenance(ctx, r.WorkerID)
	if err != nil {
		return err
	}
	if state.Mode != "active" || state.Phase != "activating" || state.Revision != revision {
		return registry.ErrRevision
	}
	enabled := true
	req.Enabled, req.ExpectedRevision, req.SourceExecution = &enabled, &current.Revision, &saved.Source
	ack, err := checkedPacketRule(ctx, client, scope, req, saved.Key+"-restore")
	if err != nil {
		return err
	}
	if ack.ID != saved.ID || !*ack.Enabled || ack.Revision != current.Revision+1 {
		return registry.ErrRevision
	}
	return e.DB.SetPacketWakeOverlay(ctx, r, revision, nil)
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
