package registry

import (
	"context"
	"encoding/json"
)

type WorkerPacketWakeRule struct {
	BindingID          string
	WorkerID           string
	SourceAttachmentID string
	TargetAttachmentID string
	Transport          string
	Port               uint16
	SourceBindingID    string
	DesiredEnabled     bool
	IntentRevision     int64
	Overlay            json.RawMessage
}

// TrackWorkerPacketWakeRule registers the exact pins before a packet wake
// mutation is sent, so Drain can query even a rule whose reply was lost.
func (s *Store) TrackWorkerPacketWakeRule(ctx context.Context, rule WorkerPacketWakeRule, enabling bool) error {
	if rule.BindingID == "" || rule.WorkerID == "" || rule.SourceAttachmentID == "" || rule.TargetAttachmentID == "" ||
		rule.SourceAttachmentID == rule.TargetAttachmentID || !oneOf(rule.Transport, "tcp", "udp") || rule.Port == 0 {
		return ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := readBinding(ctx, tx, rule.BindingID)
	if err != nil {
		return err
	}
	if b.WorkerID != rule.WorkerID || b.Status != "current" {
		return ErrGeneration
	}
	if enabling {
		if err := checkWritable(ctx, tx); err != nil {
			return err
		}
		if err := checkWorkerEntrance(ctx, tx, rule.WorkerID, "packet_wake_enable"); err != nil {
			return err
		}
	}
	if rule.SourceBindingID != "" {
		src, err := readBinding(ctx, tx, rule.SourceBindingID)
		if err != nil {
			return err
		}
		if src.WorkerID != b.WorkerID || src.RuntimeInstanceID != b.RuntimeInstanceID || src.Status != "current" {
			return ErrGeneration
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_managed_packet_rules
		(binding_id,worker_id,source_attachment_id,target_attachment_id,transport,guest_port,source_binding_id,desired_enabled,intent_revision)
		VALUES(?,?,?,?,?,?,NULLIF(?,''),?,1)
		ON CONFLICT(binding_id,source_attachment_id,target_attachment_id,transport,guest_port) DO UPDATE SET
		 source_binding_id=excluded.source_binding_id, desired_enabled=excluded.desired_enabled,
		 intent_revision=intent_revision+1, overlay_json=NULL`,
		rule.BindingID, rule.WorkerID, rule.SourceAttachmentID, rule.TargetAttachmentID, rule.Transport, rule.Port, rule.SourceBindingID, enabling); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListWorkerPacketWakeRules(ctx context.Context, worker string) ([]WorkerPacketWakeRule, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT binding_id,worker_id,source_attachment_id,target_attachment_id,transport,guest_port,COALESCE(source_binding_id,''),desired_enabled,intent_revision,COALESCE(overlay_json,'null')
		FROM worker_managed_packet_rules WHERE worker_id=? ORDER BY binding_id,source_attachment_id,target_attachment_id,transport,guest_port`, worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []WorkerPacketWakeRule
	for rows.Next() {
		var rule WorkerPacketWakeRule
		var overlay string
		if err := rows.Scan(&rule.BindingID, &rule.WorkerID, &rule.SourceAttachmentID, &rule.TargetAttachmentID, &rule.Transport, &rule.Port, &rule.SourceBindingID, &rule.DesiredEnabled, &rule.IntentRevision, &overlay); err != nil {
			return nil, err
		}
		if overlay != "null" {
			rule.Overlay = json.RawMessage(overlay)
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// Persist the exact Worker CAS before changing a rule. A concurrent owner edit
// supersedes the saved overlay and fences a delayed maintenance acknowledgement.
func (s *Store) SetPacketWakeOverlay(ctx context.Context, r WorkerPacketWakeRule, maintenanceRevision int64, value any) error {
	var raw any
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		raw = string(data)
	}
	result, err := s.writer.ExecContext(ctx, `UPDATE worker_managed_packet_rules SET overlay_json=?
	WHERE binding_id=? AND source_attachment_id=? AND target_attachment_id=? AND transport=? AND guest_port=?
	AND desired_enabled=1 AND intent_revision=? AND EXISTS(SELECT 1 FROM workers WHERE id=? AND revision=?)`,
		raw, r.BindingID, r.SourceAttachmentID, r.TargetAttachmentID, r.Transport, r.Port, r.IntentRevision, r.WorkerID, maintenanceRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrRevision
	}
	return err
}
