package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"sandbox-platform/internal/placement"
)

const (
	InventoryKnownOnly  = "known_only"
	InventoryWorkerFull = "worker_full"
)

type WorkerMaintenance struct {
	WorkerID string                     `json:"worker_id"`
	Mode     string                     `json:"mode"`
	Revision int64                      `json:"revision"`
	Phase    string                     `json:"phase"`
	Reason   string                     `json:"reason,omitempty"`
	Coverage string                     `json:"coverage"`
	Blockers []string                   `json:"blockers,omitempty"`
	Evidence *WorkerMaintenanceEvidence `json:"evidence,omitempty"`
}

type PacketWakeRuleEvidence struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Enabled  bool   `json:"enabled"`
}

type WorkerMaintenanceEvidence struct {
	ControlEpoch            string                   `json:"control_epoch"`
	Session                 placement.Session        `json:"session"`
	BudgetSequence          int64                    `json:"budget_sequence"`
	GateMode                string                   `json:"gate_mode"`
	GateRevision            uint64                   `json:"gate_revision"`
	Coverage                string                   `json:"coverage"`
	ExternalEntrancesClosed bool                     `json:"external_entrances_closed"`
	CollectorsActive        bool                     `json:"collectors_active"`
	Rules                   []PacketWakeRuleEvidence `json:"packet_wake_rules,omitempty"`
	PendingWakeOperations   []string                 `json:"pending_wake_operations,omitempty"`
	VolatileBindings        []string                 `json:"volatile_bindings,omitempty"`
	UnresolvedResources     []string                 `json:"unresolved_resources,omitempty"`
}

func readWorkerMaintenance(ctx context.Context, q queryer, id string) (WorkerMaintenance, bool, error) {
	state := WorkerMaintenance{WorkerID: id, Coverage: InventoryKnownOnly}
	var phase, reason, raw sql.NullString
	var clearing bool
	err := q.QueryRowContext(ctx, `SELECT w.desired_mode,w.revision,m.phase,m.reason,m.evidence_json,
		COALESCE(m.requires_overlay_clear,0) FROM workers w LEFT JOIN worker_maintenance m ON w.id=m.worker_id WHERE w.id=?`, id).
		Scan(&state.Mode, &state.Revision, &phase, &reason, &raw, &clearing)
	if err != nil {
		return state, false, notFound(err)
	}
	state.Phase, state.Reason = phase.String, reason.String
	if state.Phase == "" {
		state.Phase = state.Mode
	}
	if raw.Valid {
		var proof WorkerMaintenanceEvidence
		if err := json.Unmarshal([]byte(raw.String), &proof); err != nil {
			return state, false, err
		}
		state.Coverage = proof.Coverage
		state.Evidence = &proof
	}
	return state, clearing, nil
}

func workerMaintenanceBlockers(ctx context.Context, q queryer, state WorkerMaintenance) ([]string, error) {
	if !oneOf(state.Mode, "draining", "drained") && state.Phase != "activating" {
		return nil, nil
	}
	var out []string
	p := state.Evidence
	current, err := readWorkerSample(ctx, q, state.WorkerID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	fresh := err == nil && time.Since(current.oldestAt) >= 0 && time.Since(current.oldestAt) <= placement.DefaultSampleAge &&
		p != nil && p.ControlEpoch == current.ControlEpoch && p.Session == current.Session && p.BudgetSequence == current.Sequence
	if !fresh || p.GateMode != state.Mode || p.GateRevision == 0 {
		out = append(out, "managed_gate_unconfirmed")
	}
	var missingOverlays bool
	var clearing bool
	if err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT requires_overlay_clear FROM worker_maintenance
		WHERE worker_id=?),0)`, state.WorkerID).Scan(&clearing); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_bindings b
		JOIN machines m ON m.current_binding_id=b.id
		JOIN machine_lifecycle_policies p ON p.machine_id=m.id
		LEFT JOIN worker_policy_overlays o ON o.binding_id=b.id
		WHERE b.worker_id=? AND b.runtime_instance_object_id IS NOT NULL AND m.deleted_at IS NULL
		AND (o.binding_id IS NULL OR o.maintenance_revision<>? OR o.desired_revision<>p.desired_revision))`,
		state.WorkerID, state.Revision).Scan(&missingOverlays); err != nil {
		return nil, err
	}
	if missingOverlays && (state.Phase != "activating" || clearing) {
		out = append(out, "policy_overlay_pending")
	}
	if state.Phase == "activating" {
		return out, nil
	}
	// A known-only view cannot prove Worker-wide quiescence, even when
	// every platform-managed binding and rule has been accounted for.
	if !fresh || p.Coverage != InventoryWorkerFull {
		out = append(out, "inventory_known_only")
	}
	if !fresh || !p.ExternalEntrancesClosed {
		out = append(out, "worker_entrances_unconfirmed")
	}
	if !fresh || !p.CollectorsActive {
		out = append(out, "activity_unknown")
	}
	if p != nil {
		if slices.ContainsFunc(p.Rules, func(rule PacketWakeRuleEvidence) bool { return rule.Enabled }) {
			out = append(out, "packet_wake_rules_enabled")
		}
		if len(p.PendingWakeOperations) != 0 {
			out = append(out, "wake_operations_pending")
		}
		if len(p.VolatileBindings) != 0 {
			out = append(out, "volatile_bindings")
		}
		if len(p.UnresolvedResources) != 0 {
			out = append(out, "resources_unresolved")
		}
	}
	var operations, volatile bool
	err = q.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM operations WHERE worker_id=? AND state IN ('queued','dispatching','observing','recovery_required'))
		  OR EXISTS(SELECT 1 FROM placement_reservations WHERE worker_id=? AND phase IN ('dispatch_started','unknown','admitted')),
		EXISTS(SELECT 1 FROM runtime_bindings b JOIN machines m ON m.current_binding_id=b.id
			WHERE b.worker_id=? AND m.deleted_at IS NULL AND b.power_state IN ('running','standby','unknown'))`,
		state.WorkerID, state.WorkerID, state.WorkerID).Scan(&operations, &volatile)
	if err != nil {
		return nil, err
	}
	if operations {
		out = append(out, "operation_unresolved")
	}
	if volatile && !slices.Contains(out, "volatile_bindings") {
		out = append(out, "volatile_bindings")
	}
	return out, nil
}

func (s *Store) GetWorkerMaintenance(ctx context.Context, worker string) (WorkerMaintenance, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkerMaintenance{}, err
	}
	defer tx.Rollback()
	state, _, err := readWorkerMaintenance(ctx, tx, worker)
	if err != nil {
		return state, err
	}
	state.Blockers, err = workerMaintenanceBlockers(ctx, tx, state)
	if err != nil {
		return state, err
	}
	if state.Mode == "draining" && len(state.Blockers) != 0 {
		state.Phase = "draining_blocked"
	}
	return state, tx.Commit()
}

func (s *Store) SetWorkerMaintenance(ctx context.Context, worker string, expectedRevision int64, mode, reason string) (WorkerMaintenance, error) {
	if expectedRevision < 1 || strings.TrimSpace(reason) == "" || !oneOf(mode, "active", "cordoned", "draining", "drained", "decommissioned") {
		return WorkerMaintenance{}, ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return WorkerMaintenance{}, err
	}
	defer tx.Rollback()
	if err := checkWritable(ctx, tx); err != nil {
		return WorkerMaintenance{}, err
	}
	state, clearing, err := readWorkerMaintenance(ctx, tx, worker)
	if err != nil {
		return state, err
	}
	if state.Revision != expectedRevision {
		return state, ErrRevision
	}
	if state.Mode == mode {
		return state, nil
	}
	phase := mode
	switch {
	case state.Mode == "active" && mode == "cordoned":
		phase = "cordoning"
	case state.Mode == "cordoned" && mode == "draining":
		phase = "draining_blocked"
	case oneOf(state.Mode, "cordoned", "draining", "drained") && mode == "active":
		phase = "activating"
		clearing = clearing || oneOf(state.Mode, "draining", "drained")
	case state.Mode == "draining" && mode == "drained":
		state.Blockers, err = workerMaintenanceBlockers(ctx, tx, state)
		if err != nil {
			return state, err
		}
		if len(state.Blockers) != 0 {
			return state, fmt.Errorf("%w: Worker drain proof incomplete", ErrWorkerMaintenance)
		}
	case state.Mode == "drained" && mode == "decommissioned":
		// Revalidate the last complete proof and the current budget identity.
		previous := state
		previous.Mode = "draining"
		previous.Revision--
		previous.Blockers, err = workerMaintenanceBlockers(ctx, tx, previous)
		if err != nil {
			return state, err
		}
		if len(previous.Blockers) != 0 {
			return state, fmt.Errorf("%w: Worker drain proof incomplete", ErrWorkerMaintenance)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_storage_tombstones
			SELECT id, revision+1, endpoint, COALESCE((SELECT MAX(generation) FROM worker_enrollments WHERE worker_id=?),0), ?, ?
			FROM workers WHERE id=?`, worker, reason, time.Now().UnixMilli(), worker); err != nil {
			return state, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_enrollments SET status='revoked',revoked_at=?
			WHERE worker_id=? AND status='active'`, time.Now().UnixMilli(), worker); err != nil {
			return state, err
		}
	default:
		return state, fmt.Errorf("%w: cannot move Worker from %s to %s", ErrWorkerMaintenance, state.Mode, mode)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET desired_mode=?,revision=revision+1 WHERE id=?`, mode, worker); err != nil {
		return state, err
	}
	var evidence any
	if mode == "drained" {
		raw, err := json.Marshal(state.Evidence)
		if err != nil {
			return state, err
		}
		evidence = string(raw)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_maintenance VALUES(?,?,?,?,?,?)
		ON CONFLICT(worker_id) DO UPDATE SET revision=excluded.revision,phase=excluded.phase,
		reason=excluded.reason,requires_overlay_clear=excluded.requires_overlay_clear,evidence_json=excluded.evidence_json`,
		worker, expectedRevision+1, phase, reason, clearing, evidence); err != nil {
		return state, err
	}
	if err := AppendEvent(ctx, tx, "worker_maintenance", "", "", map[string]any{
		"worker_id": worker, "mode": mode, "phase": phase, "revision": expectedRevision + 1, "reason": reason,
	}, true); err != nil {
		return state, err
	}
	if err := tx.Commit(); err != nil {
		return state, err
	}
	return s.GetWorkerMaintenance(ctx, worker)
}

func (s *Store) RecordWorkerMaintenanceEvidence(ctx context.Context, worker string, expectedRevision int64, proof WorkerMaintenanceEvidence) (WorkerMaintenance, error) {
	if !oneOf(proof.Coverage, InventoryKnownOnly, InventoryWorkerFull) || !proof.Session.Valid() ||
		proof.GateRevision == 0 || proof.BudgetSequence < 1 {
		return WorkerMaintenance{}, ErrInvalid
	}
	for _, rule := range proof.Rules {
		if rule.ID == "" || rule.Revision < 1 {
			return WorkerMaintenance{}, ErrInvalid
		}
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return WorkerMaintenance{}, err
	}
	defer tx.Rollback()
	state, _, err := readWorkerMaintenance(ctx, tx, worker)
	if err != nil {
		return state, err
	}
	if state.Revision != expectedRevision || proof.GateMode != state.Mode {
		return state, ErrRevision
	}
	if state.Phase == state.Mode && !oneOf(state.Mode, "cordoned", "draining") {
		return state, ErrOperationState
	}
	control, err := readControl(ctx, tx)
	if err != nil {
		return state, err
	}
	sample, err := readWorkerSample(ctx, tx, worker)
	if err != nil {
		return state, err
	}
	if proof.ControlEpoch != control.ControlEpoch || proof.ControlEpoch != sample.ControlEpoch || sample.Session != proof.Session ||
		sample.Sequence != proof.BudgetSequence || time.Since(sample.oldestAt) > placement.DefaultSampleAge {
		return state, ErrStaleObservation
	}
	if state.Evidence != nil && state.Evidence.Session == proof.Session &&
		(proof.GateRevision < state.Evidence.GateRevision || proof.BudgetSequence < state.Evidence.BudgetSequence) {
		return state, ErrStaleObservation
	}
	state.Evidence = &proof
	state.Blockers, err = workerMaintenanceBlockers(ctx, tx, state)
	if err != nil {
		return state, err
	}
	switch {
	case state.Mode == "cordoned":
		state.Phase = "cordoned"
	case state.Mode == "draining" && len(state.Blockers) == 0:
		state.Phase = "draining"
	case state.Mode == "draining":
		state.Phase = "draining_blocked"
	case state.Mode == "active" && len(state.Blockers) == 0:
		state.Phase = "active"
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return state, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worker_maintenance SET phase=?,evidence_json=? WHERE worker_id=? AND revision=?`,
		state.Phase, string(raw), worker, expectedRevision); err != nil {
		return state, err
	}
	state.Coverage = proof.Coverage
	return state, tx.Commit()
}
