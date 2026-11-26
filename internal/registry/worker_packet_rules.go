package registry

import (
	"context"
)

type WorkerPacketWakeRule struct {
	BindingID          string
	WorkerID           string
	SourceAttachmentID string
	TargetAttachmentID string
	Transport          string
	Port               uint16
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_managed_packet_rules VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		rule.BindingID, rule.WorkerID, rule.SourceAttachmentID, rule.TargetAttachmentID, rule.Transport, rule.Port); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListWorkerPacketWakeRules(ctx context.Context, worker string) ([]WorkerPacketWakeRule, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT binding_id,worker_id,source_attachment_id,target_attachment_id,transport,guest_port
		FROM worker_managed_packet_rules WHERE worker_id=? ORDER BY binding_id,source_attachment_id,target_attachment_id,transport,guest_port`, worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []WorkerPacketWakeRule
	for rows.Next() {
		var rule WorkerPacketWakeRule
		if err := rows.Scan(&rule.BindingID, &rule.WorkerID, &rule.SourceAttachmentID, &rule.TargetAttachmentID, &rule.Transport, &rule.Port); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}
