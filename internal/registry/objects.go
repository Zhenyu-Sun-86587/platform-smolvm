package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

func ensureJSON(raw json.RawMessage, fallback string) (string, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("%w: invalid JSON", ErrInvalid)
	}
	return string(raw), nil
}

func uniqueConflict(err error) error {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) &&
		(sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique || sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey) {
		return ErrConflict
	}
	return err
}

func rawValue(raw json.RawMessage) any {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return string(raw)
}

func (s *Store) EnsureTemplate(ctx context.Context, t Template) error {
	if t.ID == "" || t.Name == "" || t.Version < 1 || t.ImageDigest == "" {
		return fmt.Errorf("%w: template id, name, version and image digest are required", ErrInvalid)
	}
	spec, err := ensureJSON(t.Spec, "{}")
	if err != nil {
		return err
	}
	capabilities, err := ensureJSON(t.Capabilities, "{}")
	if err != nil {
		return err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO templates
		(id, name, version, image_digest, guest_release, spec_json, capabilities_json, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, t.ID, t.Name, t.Version, t.ImageDigest,
		nullable(t.GuestRelease), spec, capabilities, timeValue(t.VerifiedAt)); err != nil {
		return uniqueConflict(err)
	}
	existing, err := readTemplate(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	if existing.Name != t.Name || existing.Version != t.Version || existing.ImageDigest != t.ImageDigest ||
		existing.GuestRelease != t.GuestRelease || !sameJSON(existing.Spec, []byte(spec)) ||
		!sameJSON(existing.Capabilities, []byte(capabilities)) {
		return fmt.Errorf("%w: template %s differs from its immutable definition", ErrConflict, t.ID)
	}
	return tx.Commit()
}

const templateColumns = `id, name, version, image_digest, COALESCE(guest_release, ''), spec_json, capabilities_json, verified_at`

func scanTemplate(row scanner) (Template, error) {
	var t Template
	var spec, capabilities string
	var verified sql.NullInt64
	err := row.Scan(&t.ID, &t.Name, &t.Version, &t.ImageDigest, &t.GuestRelease,
		&spec, &capabilities, &verified)
	t.Spec = json.RawMessage(spec)
	t.Capabilities = json.RawMessage(capabilities)
	t.VerifiedAt = timePtr(verified)
	return t, notFound(err)
}

func readTemplate(ctx context.Context, q queryer, id string) (Template, error) {
	return scanTemplate(q.QueryRowContext(ctx, "SELECT "+templateColumns+" FROM templates WHERE id = ?", id))
}

func (s *Store) GetTemplate(ctx context.Context, id string) (Template, error) {
	return readTemplate(ctx, s.reader, id)
}

func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT "+templateColumns+" FROM templates ORDER BY name, version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	templates := []Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

const machineColumns = `m.id, m.name, m.template_id, m.revision, m.binding_generation,
	m.desired_power, m.spec_json, m.idle_policy_json, m.keep_running, m.last_user_activity_at,
	m.deleted_at, COALESCE(m.current_binding_id, ''), COALESCE(l.operation_id, '')`

func scanMachine(row scanner) (Machine, string, error) {
	var m Machine
	var bindingID, spec, idle string
	var lastActivity, deleted sql.NullInt64
	err := row.Scan(&m.ID, &m.Name, &m.TemplateID, &m.Revision, &m.BindingGeneration,
		&m.DesiredPower, &spec, &idle, &m.KeepRunning, &lastActivity, &deleted, &bindingID, &m.ActiveOperationID)
	m.Spec = json.RawMessage(spec)
	m.IdlePolicy = json.RawMessage(idle)
	m.LastUserActivityAt = timePtr(lastActivity)
	m.DeletedAt = timePtr(deleted)
	m.ObservedState = "unknown"
	m.Availability = "not_ready"
	if m.DeletedAt != nil {
		m.ObservedState = "deleted"
	}
	return m, bindingID, notFound(err)
}

func attachBinding(m *Machine, b Binding) {
	m.CurrentBinding = &b
	if m.DeletedAt == nil {
		m.ObservedState = b.ObservedState
		m.Availability = b.Availability
		m.ObservedAt = b.ObservedAt
	}
}

func readMachine(ctx context.Context, q queryer, id string) (Machine, error) {
	m, bindingID, err := scanMachine(q.QueryRowContext(ctx, "SELECT "+machineColumns+`
		FROM machines m LEFT JOIN operation_locks l ON l.machine_id = m.id WHERE m.id = ?`, id))
	if err != nil || bindingID == "" {
		return m, err
	}
	b, err := readBinding(ctx, q, bindingID)
	if err != nil {
		return Machine{}, err
	}
	attachBinding(&m, b)
	return m, nil
}

// GetMachine returns tombstone records as well as active machines.
func (s *Store) GetMachine(ctx context.Context, id string) (Machine, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Machine{}, err
	}
	defer tx.Rollback()
	m, err := readMachine(ctx, tx, id)
	if err != nil {
		return Machine{}, err
	}
	return m, tx.Commit()
}

func (s *Store) ListMachines(ctx context.Context) ([]Machine, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT "+machineColumns+`
		FROM machines m LEFT JOIN operation_locks l ON l.machine_id = m.id
		WHERE m.deleted_at IS NULL ORDER BY m.name, m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	machines := []Machine{}
	byBinding := make(map[string]int)
	for rows.Next() {
		m, bindingID, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		if bindingID != "" {
			byBinding[bindingID] = len(machines)
		}
		machines = append(machines, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	bindings, err := readBindings(ctx, tx, "WHERE status = 'current'")
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if i, ok := byBinding[b.ID]; ok {
			attachBinding(&machines[i], b)
		}
	}
	return machines, tx.Commit()
}

const bindingColumns = `id, machine_id, generation, runtime_name, status, task_namespace, spec_hash,
	observed_state, native_state, availability, COALESCE(runtime_identity_json, ''), observed_at,
	COALESCE(runtime_pause_id, ''), COALESCE(pause_status, ''), ports_json, guest_management_status,
	guest_management_evidence_json, COALESCE(state_reason_json, ''),
	COALESCE(runtime_instance_id,''), COALESCE(runtime_bundle_id,''), COALESCE(runtime_instance_object_id,''),
	COALESCE(incarnation_id,''), COALESCE(execution_epoch,0), power_state, health_json,
	COALESCE(capability_profile_id,''), COALESCE(last_capture_id,'')`

func scanBinding(row scanner) (Binding, error) {
	var b Binding
	var identity, ports, guestEvidence, reason, health string
	var observed sql.NullInt64
	err := row.Scan(&b.ID, &b.MachineID, &b.Generation, &b.RuntimeName, &b.Status, &b.TaskNamespace,
		&b.SpecHash, &b.ObservedState, &b.NativeState, &b.Availability, &identity, &observed, &b.RuntimePauseID,
		&b.PauseStatus, &ports, &b.GuestManagementStatus, &guestEvidence, &reason,
		&b.RuntimeInstanceID, &b.RuntimeBundleID, &b.RuntimeInstanceObjectID, &b.IncarnationID,
		&b.ExecutionEpoch, &b.PowerState, &health, &b.CapabilityProfileID, &b.LastCaptureID)
	b.Health = json.RawMessage(health)
	b.RuntimeIdentity = json.RawMessage(identity)
	b.ObservedAt = timePtr(observed)
	b.Ports = json.RawMessage(ports)
	b.GuestManagementEvidence = json.RawMessage(guestEvidence)
	b.StateReason = json.RawMessage(reason)
	return b, notFound(err)
}

func readBinding(ctx context.Context, q queryer, id string) (Binding, error) {
	return scanBinding(q.QueryRowContext(ctx, "SELECT "+bindingColumns+" FROM runtime_bindings WHERE id = ?", id))
}

func readBindings(ctx context.Context, q queryer, clause string, args ...any) ([]Binding, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+bindingColumns+" FROM runtime_bindings "+clause+" ORDER BY machine_id, generation", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := []Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

func (s *Store) GetBinding(ctx context.Context, id string) (Binding, error) {
	return readBinding(ctx, s.reader, id)
}

func (s *Store) ListBindings(ctx context.Context, machineID string) ([]Binding, error) {
	if machineID == "" {
		return readBindings(ctx, s.reader, "")
	}
	return readBindings(ctx, s.reader, "WHERE machine_id = ?", machineID)
}

func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	normalizedA, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	normalizedB, err := canonicalJSON(b)
	return err == nil && bytes.Equal(normalizedA, normalizedB)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: invalid JSON", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (s *Store) ObserveMachine(ctx context.Context, bindingID string, observation Observation) error {
	return s.ObserveOwnedMachine(ctx, bindingID, "", observation)
}

// An operation can project its verified wake while it holds the machine lock.
func (s *Store) ObserveOwnedMachine(ctx context.Context, bindingID, operationID string, observation Observation) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	binding, err := readBinding(ctx, tx, bindingID)
	if err != nil {
		return err
	}
	m, err := readMachine(ctx, tx, binding.MachineID)
	if err != nil {
		return err
	}
	if m.DeletedAt != nil || m.CurrentBinding == nil || m.CurrentBinding.ID != bindingID || m.ActiveOperationID != operationID {
		return ErrStaleObservation
	}
	changed, err := persistObservation(ctx, tx, binding, observation)
	if err != nil {
		return err
	}
	if changed {
		if err := appendMachineEvent(ctx, tx, binding.MachineID, "", false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func persistObservation(ctx context.Context, tx *sql.Tx, old Binding, o Observation) (bool, error) {
	for _, field := range []json.RawMessage{o.RuntimeIdentity, o.ExpectedRuntimeIdentity, o.Ports, o.GuestManagementEvidence, o.StateReason} {
		if len(field) > 0 && !json.Valid(field) {
			return false, fmt.Errorf("%w: invalid observation JSON", ErrInvalid)
		}
	}
	if (o.ExpectedExecutionEpoch != nil && *o.ExpectedExecutionEpoch != old.ExecutionEpoch) ||
		(o.ExpectedPowerState != "" && o.ExpectedPowerState != old.PowerState) ||
		(o.ExpectedCaptureID != "" && o.ExpectedCaptureID != old.LastCaptureID) ||
		(len(o.ExpectedRuntimeIdentity) > 0 && !sameJSON(o.ExpectedRuntimeIdentity, old.RuntimeIdentity)) {
		return false, ErrStaleObservation
	}
	observedAt := o.ObservedAt
	if observedAt == nil {
		now := time.Now().UTC()
		observedAt = &now
	}
	if old.ObservedAt != nil && observedAt.UnixMilli() < old.ObservedAt.UnixMilli() {
		return false, ErrStaleObservation
	}
	if o.ObservedState == "" {
		o.ObservedState = old.ObservedState
		o.NativeState = old.NativeState
		o.RuntimeIdentity = old.RuntimeIdentity
		o.StateReason = old.StateReason
	}
	if o.Availability == "" {
		o.Availability = old.Availability
	}
	if o.RuntimePauseID == "" {
		o.RuntimePauseID = old.RuntimePauseID
	}
	if o.PauseStatus == "" {
		o.PauseStatus = old.PauseStatus
	}
	if len(o.Ports) == 0 {
		o.Ports = old.Ports
	}
	if o.GuestManagementStatus == "" {
		o.GuestManagementStatus = old.GuestManagementStatus
		o.GuestManagementEvidence = old.GuestManagementEvidence
		if !sameJSON(o.RuntimeIdentity, old.RuntimeIdentity) {
			o.GuestManagementStatus = "unknown"
			o.GuestManagementEvidence = json.RawMessage("{}")
		}
	}
	if len(o.GuestManagementEvidence) == 0 {
		o.GuestManagementEvidence = json.RawMessage("{}")
	}
	if !oneOf(o.GuestManagementStatus, "unknown", "degraded", "ready") ||
		!oneOf(o.PauseStatus, "", "active", "consumed", "unknown") {
		return false, fmt.Errorf("%w: invalid guest management or pause status", ErrInvalid)
	}

	nativeChanged := false
	health := old.Health
	if o.Native != nil || o.WorkloadError != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(health, &fields); err != nil {
			return false, fmt.Errorf("decode persisted workload health: %w", err)
		}
		if o.Native != nil {
			native := o.Native
			ref := native.Execution
			initialStopped := native.VerifiedLaunch && native.PowerState == PowerStopped &&
				old.ExecutionEpoch == 0 && ref.ExecutionEpoch == 0 && ref.IncarnationID == ""
			nextLaunch := native.VerifiedLaunch && native.PowerState == PowerRunning && old.PowerState != PowerHibernated &&
				ref.ExecutionEpoch == old.ExecutionEpoch+1 && ref.IncarnationID != old.IncarnationID
			nextRestore := native.VerifiedRestore && native.PowerState == PowerRunning &&
				ref.ExecutionEpoch > old.ExecutionEpoch && ref.IncarnationID != old.IncarnationID
			hibernated := native.PowerState == PowerHibernated && native.HibernatedCapture != "" &&
				old.ExecutionEpoch > 0 && old.ExecutionRef() == ref
			stopped := native.VerifiedStop && native.PowerState == PowerStopped &&
				old.PowerState == PowerRunning && old.IncarnationID != "" && old.ExecutionRef() == ref
			if o.ExpectedExecutionEpoch == nil || old.RuntimeBundleID == "" || ref.InstanceID == "" ||
				(old.RuntimeInstanceObjectID != "" && old.RuntimeInstanceObjectID != ref.InstanceID) ||
				(!initialStopped && (ref.IncarnationID == "" || ref.ExecutionEpoch < 1)) ||
				(native.VerifiedLaunch && !initialStopped && !nextLaunch) ||
				(native.VerifiedRestore && !nextRestore) ||
				(native.VerifiedStop && !stopped) ||
				(!nextLaunch && !nextRestore && old.ExecutionEpoch > 0 && old.ExecutionRef() != ref) {
				return false, ErrStaleObservation
			}
			if !initialStopped && !hibernated && !stopped && native.PowerState != PowerRunning && native.PowerState != PowerStandby {
				return false, fmt.Errorf("%w: invalid native power evidence", ErrInvalid)
			}
			var executionEpoch any
			if ref.ExecutionEpoch > 0 {
				executionEpoch = ref.ExecutionEpoch
			}
			_, err := tx.ExecContext(ctx, `UPDATE runtime_bindings SET runtime_instance_object_id=?,
				incarnation_id=?, execution_epoch=?, power_state=?, capability_profile_id=? WHERE id=?`,
				ref.InstanceID, nullable(ref.IncarnationID), executionEpoch, native.PowerState, nullable(native.VerifiedStandbyProfile), old.ID)
			if err != nil {
				return false, err
			}
			if hibernated {
				if _, err := tx.ExecContext(ctx, "UPDATE runtime_bindings SET last_capture_id=? WHERE id=?", native.HibernatedCapture, old.ID); err != nil {
					return false, err
				}
			}
			fields["vm"], _ = json.Marshal(native.PowerState)
			fields["guest_management"], _ = json.Marshal(o.GuestManagementStatus)
			if native.ForkCapabilities != nil {
				fields["native_fork_capabilities"], _ = json.Marshal(native.ForkCapabilities)
			}
			if native.StopBarrier != nil {
				fields["native_stop_barrier"], _ = json.Marshal(*native.StopBarrier)
			} else if old.ExecutionRef() != ref {
				delete(fields, "native_stop_barrier")
			}
			if stopped {
				delete(fields, "native_fork_capabilities")
				delete(fields, "native_stop_barrier")
				delete(fields, "workload")
				delete(fields, "workload_error")
			}
			nativeChanged = old.ExecutionRef() != ref || old.PowerState != native.PowerState || old.CapabilityProfileID != native.VerifiedStandbyProfile
		}
		if o.WorkloadError != nil {
			if *o.WorkloadError == "" {
				delete(fields, "workload")
				delete(fields, "workload_error")
			} else {
				fields["workload"] = json.RawMessage(`"failed"`)
				detail, err := json.Marshal(map[string]string{"code": WorkloadStartFailed, "message": *o.WorkloadError})
				if err != nil {
					return false, err
				}
				fields["workload_error"] = detail
			}
		}
		var err error
		health, err = json.Marshal(fields)
		if err != nil {
			return false, err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE runtime_bindings SET observed_state = ?, native_state = ?, availability = ?,
		runtime_identity_json = ?, observed_at = ?, runtime_pause_id = ?, pause_status = ?, ports_json = ?,
		guest_management_status = ?, guest_management_evidence_json = ?, state_reason_json = ?, health_json = ?
		WHERE id = ?`, o.ObservedState, o.NativeState, o.Availability, rawValue(o.RuntimeIdentity), observedAt.UnixMilli(),
		nullable(o.RuntimePauseID), nullable(o.PauseStatus), string(o.Ports), o.GuestManagementStatus,
		string(o.GuestManagementEvidence), rawValue(o.StateReason), string(health), old.ID)
	if err != nil {
		return false, err
	}
	changed := nativeChanged || o.ObservedState != old.ObservedState || o.NativeState != old.NativeState || o.Availability != old.Availability ||
		o.RuntimePauseID != old.RuntimePauseID || o.PauseStatus != old.PauseStatus ||
		o.GuestManagementStatus != old.GuestManagementStatus || !sameJSON(o.RuntimeIdentity, old.RuntimeIdentity) ||
		!sameJSON(o.Ports, old.Ports) || !sameJSON(o.GuestManagementEvidence, old.GuestManagementEvidence) ||
		!sameJSON(o.StateReason, old.StateReason) || !sameJSON(health, old.Health)
	return changed, nil
}

func oneOf(s string, values ...string) bool {
	for _, value := range values {
		if s == value {
			return true
		}
	}
	return false
}

func validateLabel(label string, limit int) bool {
	return strings.TrimSpace(label) != "" && len(label) <= limit && strings.IndexByte(label, 0) == -1
}
