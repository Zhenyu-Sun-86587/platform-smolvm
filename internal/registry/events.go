package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const (
	eventRetention = 7 * 24 * time.Hour
	auditRetention = 90 * 24 * time.Hour
)

// AppendEvent writes a redacted state change as part of its owner transaction.
func AppendEvent(ctx context.Context, tx *sql.Tx, kind, machineID, operationID string, payload any, audit bool) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resource := ResourceEvent{Kind: kind, Name: kind + ".updated", Audience: "read"}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	field := func(key string) (value string) {
		_ = json.Unmarshal(fields[key], &value)
		return value
	}
	switch kind {
	case "machine":
		resource.ID, resource.Name = machineID, "machine.state_changed"
	case "operation":
		resource.ID, resource.Name = operationID, "operation.stage_changed"
	case "auth":
		resource.ID, resource.Audience = field("credential_id"), "admin"
		if resource.ID != "" {
			resource.Kind = "credential"
		}
		if action := field("action"); action != "" {
			resource.Name = "auth." + action
			if action == "credential_revoked" {
				resource.Name = "credential.revoked"
			}
		}
	case "maintenance":
		resource.Audience = "admin"
	default:
		resource.ID = field(kind + "_id")
		if state := field("state"); state != "" {
			resource.Name = kind + "." + state
		}
	}
	return AppendResourceEvent(ctx, tx, kind, machineID, operationID, resource, raw, audit)
}

type ResourceEvent struct {
	Kind, ID, Name, Audience, OwnerID string
}

func AppendResourceEvent(ctx context.Context, tx *sql.Tx, category, machineID, operationID string, resource ResourceEvent, payload json.RawMessage, audit bool) error {
	if resource.Kind == "" || resource.Name == "" ||
		(resource.Audience == "owner") != (resource.OwnerID != "") {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events
		(control_epoch,type,machine_id,operation_id,payload_json,created_at,audit,resource_kind,resource_id,event_name,audience,audience_owner)
		SELECT control_epoch,?,?,?,?,?,?,?,?,?,?,? FROM meta WHERE id=1`,
		category, nullable(machineID), nullable(operationID), string(payload), time.Now().UnixMilli(), audit,
		resource.Kind, resource.ID, resource.Name, resource.Audience, nullable(resource.OwnerID))
	return err
}

func appendMachineEvent(ctx context.Context, tx *sql.Tx, machineID, operationID string, audit bool) error {
	m, err := readMachine(ctx, tx, machineID)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"machine_id": m.ID, "revision": m.Revision, "observed_state": m.ObservedState,
		"binding_generation": m.BindingGeneration, "availability": m.Availability,
		"desired_power": m.DesiredPower, "observed_at": m.ObservedAt, "operation_id": m.ActiveOperationID,
	}
	return AppendEvent(ctx, tx, "machine", machineID, operationID, payload, audit)
}

func appendOperationEvent(ctx context.Context, tx *sql.Tx, op Operation) error {
	payload := map[string]any{
		"operation_id": op.ID, "kind": op.Kind, "state": op.State,
		"stage": op.Stage, "effect": op.Effect, "binding_generation": op.BindingGeneration, "revision": op.Version,
	}
	if op.Kind == rt.VolumeOperation && op.MachineID == "" && op.PrincipalID != "" {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return AppendResourceEvent(ctx, tx, "operation", "", op.ID,
			ResourceEvent{Kind: "operation", ID: op.ID, Name: "operation.stage_changed", Audience: "owner", OwnerID: op.PrincipalID}, raw, false)
	}
	return AppendEvent(ctx, tx, "operation", op.MachineID, op.ID, payload, op.Kind == "delete" || op.Kind == "rollback")
}

func eventID(epoch string, seq int64) string {
	return fmt.Sprintf("%s:%d", epoch, seq)
}

func (s *Store) EventWatermark(ctx context.Context) (string, error) {
	var epoch string
	var seq int64
	err := s.reader.QueryRowContext(ctx, `SELECT control_epoch,
		COALESCE((SELECT MAX(seq) FROM events WHERE control_epoch = meta.control_epoch), 0)
		FROM meta WHERE id = 1`).Scan(&epoch, &seq)
	if err != nil {
		return "", err
	}
	return eventID(epoch, seq), nil
}

func (s *Store) Events(ctx context.Context, cursor string, limit int) ([]Event, string, error) {
	if limit <= 0 || limit > 1000 {
		return nil, "", fmt.Errorf("%w: event limit out of range", ErrInvalid)
	}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	control, err := readControl(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	threshold := time.Now().Add(-eventRetention).UnixMilli()
	var seq int64
	if cursor != "" {
		cursorEpoch, cursorSeq, ok := strings.Cut(cursor, ":")
		if !ok || cursorEpoch != control.ControlEpoch {
			return nil, "", ErrCursorExpired
		}
		seq, err = strconv.ParseInt(cursorSeq, 10, 64)
		if err != nil || seq < 0 || cursorSeq != strconv.FormatInt(seq, 10) {
			return nil, "", ErrCursorExpired
		}
		if seq != 0 {
			var createdAt int64
			err := tx.QueryRowContext(ctx, `SELECT created_at FROM events WHERE control_epoch = ? AND seq = ?`,
				control.ControlEpoch, seq).Scan(&createdAt)
			if errors.Is(err, sql.ErrNoRows) || err == nil && createdAt < threshold {
				return nil, "", ErrCursorExpired
			}
			if err != nil {
				return nil, "", err
			}
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT seq, type, COALESCE(machine_id, ''), COALESCE(operation_id, ''),
		payload_json,created_at,resource_kind,resource_id,event_name,schema_version,audience,COALESCE(audience_owner,'')
		FROM events WHERE control_epoch = ? AND seq > ? AND created_at >= ?
		ORDER BY seq LIMIT ?`, control.ControlEpoch, seq, threshold, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		var payload string
		var created int64
		if err := rows.Scan(&event.Seq, &event.Type, &event.MachineID, &event.OperationID, &payload, &created,
			&event.ResourceKind, &event.ResourceID, &event.Name, &event.SchemaVersion, &event.Audience, &event.AudienceOwner); err != nil {
			return nil, "", err
		}
		if event.Audience == "legacy" {
			event.ResourceKind, event.Name = event.Type, event.Type+".updated"
			switch event.Type {
			case "machine":
				event.ResourceID = event.MachineID
			case "operation":
				event.ResourceID = event.OperationID
			}
		}
		event.ControlEpoch = control.ControlEpoch
		event.ID = eventID(event.ControlEpoch, event.Seq)
		event.Payload = json.RawMessage(payload)
		event.CreatedAt = time.UnixMilli(created).UTC()
		events = append(events, event)
		seq = event.Seq
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return events, eventID(control.ControlEpoch, seq), tx.Commit()
}

func (s *Store) PruneEvents(ctx context.Context) error {
	now := time.Now()
	_, err := s.writer.ExecContext(ctx, `DELETE FROM events
		WHERE (audit = 0 AND created_at < ?) OR (audit = 1 AND created_at < ?)`,
		now.Add(-eventRetention).UnixMilli(), now.Add(-auditRetention).UnixMilli())
	return err
}
