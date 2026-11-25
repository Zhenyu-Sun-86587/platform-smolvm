package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	rt "sandbox-platform/internal/runtime"
)

type LifecycleSettings struct {
	Supported       bool                `json:"supported"`
	DesiredRevision int64               `json:"desired_revision"`
	AppliedRevision int64               `json:"applied_revision"`
	Desired         rt.LifecyclePolicy  `json:"desired"`
	Overrides       json.RawMessage     `json:"overrides"`
	Applied         *rt.LifecyclePolicy `json:"applied,omitempty"`
	Source          string              `json:"source"`
	DefaultRevision int64               `json:"default_revision"`
	LastOperation   string              `json:"operation_id"`
	Worker          json.RawMessage     `json:"worker,omitempty"`
	LastError       *string             `json:"last_error,omitempty"`
}

func readPolicy(ctx context.Context, q queryer, machine string) (LifecycleSettings, error) {
	var p LifecycleSettings
	var raw, overrides string
	var applied, worker *string
	err := q.QueryRowContext(ctx, `SELECT desired_revision,applied_revision,desired_json,overrides_json,applied_json,source,default_revision,last_operation,worker_status,last_error
        FROM machine_lifecycle_policies WHERE machine_id=?`, machine).Scan(&p.DesiredRevision, &p.AppliedRevision, &raw, &overrides, &applied, &p.Source, &p.DefaultRevision, &p.LastOperation, &worker, &p.LastError)
	if err != nil {
		return p, err
	}
	p.Overrides = json.RawMessage(overrides)
	p.Desired, err = rt.DefaultLifecyclePolicy().WithPatch(json.RawMessage(raw))
	if err != nil {
		return p, err
	}
	if applied != nil {
		a, err := rt.DefaultLifecyclePolicy().WithPatch(json.RawMessage(*applied))
		if err != nil {
			return p, err
		}
		p.Applied = &a
	}
	if worker != nil {
		p.Worker = json.RawMessage(*worker)
	}
	return p, nil
}
func (s *Store) LifecyclePolicy(ctx context.Context, machine string) (LifecycleSettings, error) {
	return readPolicy(ctx, s.reader, machine)
}

type lifecycleInput struct {
	Lifecycle json.RawMessage `json:"lifecycle_policy"`
	Idle      *struct {
		Enabled bool `json:"enabled"`
		Timeout int  `json:"timeout_seconds"`
	} `json:"idle_policy"`
	Keep *bool `json:"keep_running"`
}

// LifecycleOverrides returns the fields specified in a create, fork or configure
// request. Legacy fields use the same effective policy as their JSON equivalent.
func LifecycleOverrides(requested json.RawMessage) (json.RawMessage, string, error) {
	var spec lifecycleInput
	if len(requested) > 0 {
		if err := json.Unmarshal(requested, &spec); err != nil {
			return nil, "", err
		}
	}
	if spec.Idle != nil && spec.Lifecycle != nil {
		return nil, "", ErrInvalid
	}
	patch := map[string]any{}
	source := ""
	if spec.Lifecycle != nil {
		if err := json.Unmarshal(spec.Lifecycle, &patch); err != nil || patch == nil {
			return nil, "", ErrInvalid
		}
		source = "override"
	}
	if spec.Idle != nil {
		patch["auto_standby"] = false
		patch["auto_hibernate"] = spec.Idle.Enabled
		if spec.Idle.Timeout > 0 {
			patch["hibernate_after_seconds"] = spec.Idle.Timeout
		}
		source = "legacy"
	}
	if spec.Keep != nil {
		patch["keep_running"] = *spec.Keep
		if source == "" {
			source = "override"
		}
	}
	encoded, err := json.Marshal(patch)
	return encoded, source, err
}

func SeedLifecycle(ctx context.Context, tx *sql.Tx, machine, parent string, requested json.RawMessage, op string) error {
	override, overrideSource, err := LifecycleOverrides(requested)
	if err != nil {
		return err
	}
	var defaults struct {
		Revision       int64           `json:"revision"`
		PolicyRevision int64           `json:"default_lifecycle_revision"`
		Policy         json.RawMessage `json:"default_lifecycle_policy"`
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT settings_json FROM meta WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), &defaults); err != nil {
		return err
	}
	value, e := rt.DefaultLifecyclePolicy().WithPatch(defaults.Policy)
	if e != nil {
		return e
	}
	source := "default"
	inherited := json.RawMessage("{}")
	revision := defaults.PolicyRevision
	if revision == 0 {
		revision = 1
	}
	if parent != "" {
		p, err := readPolicy(ctx, tx, parent)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			value = p.Desired
			inherited = p.Overrides
			revision = p.DefaultRevision
			source = "parent"
		}
	}
	if overrideSource != "" {
		source = overrideSource
		value, err = value.WithPatch(override)
		if err != nil {
			return err
		}
	}
	bytes, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO machine_lifecycle_policies(machine_id,desired_revision,desired_json,overrides_json,source,default_revision,last_operation)
        VALUES(?,1,?,json_patch(?,?),?,?,?)`, machine, string(bytes), string(inherited), string(override), source, revision, op); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE machines SET keep_running=? WHERE id=?", value.KeepRunning, machine)
	return err
}
func (s *Store) SetLifecycleDesired(ctx context.Context, machine, op, source string, revision, defaultRev int64, patch json.RawMessage) (LifecycleSettings, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleSettings{}, err
	}
	defer tx.Rollback()
	p, err := readPolicy(ctx, tx, machine)
	if err != nil {
		return p, err
	}
	if p.LastOperation == op {
		return p, nil
	}
	if revision != p.DesiredRevision || p.AppliedRevision != p.DesiredRevision {
		return p, ErrRevision
	}
	policy, err := p.Desired.WithPatch(patch)
	if err != nil {
		return p, ErrInvalid
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE machine_lifecycle_policies SET desired_revision=desired_revision+1,desired_json=?,overrides_json=json_patch(overrides_json,?),last_operation=?,source=?,default_revision=?,last_error=NULL WHERE machine_id=?`,
		string(raw), string(patch), op, source, defaultRev, machine); err != nil {
		return p, err
	}
	p, err = readPolicy(ctx, tx, machine)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
