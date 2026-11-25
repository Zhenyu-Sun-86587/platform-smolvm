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
	_, err = tx.ExecContext(ctx, `INSERT INTO events
		(control_epoch, type, machine_id, operation_id, payload_json, created_at, audit)
		SELECT control_epoch, ?, ?, ?, ?, ?, ? FROM meta WHERE id = 1`,
		kind, nullable(machineID), nullable(operationID), string(raw), time.Now().UnixMilli(), audit)
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
		"stage": op.Stage, "effect": op.Effect, "binding_generation": op.BindingGeneration,
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
		payload_json, created_at FROM events WHERE control_epoch = ? AND seq > ? AND created_at >= ?
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
		if err := rows.Scan(&event.Seq, &event.Type, &event.MachineID, &event.OperationID, &payload, &created); err != nil {
			return nil, "", err
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
