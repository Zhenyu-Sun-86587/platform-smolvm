package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	rt "sandbox-platform/internal/runtime"
)

type PolicySelection struct {
	MachineID        string `json:"machine_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}
type PolicyApplicationTarget struct {
	PolicySelection
	PolicyRevision *int64  `json:"policy_revision,omitempty"`
	State          string  `json:"state"`
	Reason         *string `json:"reason,omitempty"`
}
type PolicyApplication struct {
	ID              string                    `json:"application_id"`
	DefaultRevision int64                     `json:"default_policy_revision"`
	Policy          rt.LifecyclePolicy        `json:"policy"`
	Complete        bool                      `json:"complete"`
	Targets         []PolicyApplicationTarget `json:"machines"`
}

// ReservePolicyApplication resolves defaults and records every selected machine
// in the caller's idempotent acceptance transaction.
func ReservePolicyApplication(ctx context.Context, tx *sql.Tx, requested int64, selections []PolicySelection, supported func(Binding) bool) (string, error) {
	var rev int64
	var encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(settings_json,'$.default_lifecycle_revision'),1),
            coalesce(json_extract(settings_json,'$.default_lifecycle_policy'),'{}') FROM meta WHERE id=1`).Scan(&rev, &encoded)
	if err != nil {
		return "", err
	}
	if rev != requested {
		return "", ErrRevision
	}
	policy, err := rt.DefaultLifecyclePolicy().WithPatch(encoded)
	if err != nil {
		return "", err
	}
	value, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	id, err := NewID("policy-app-")
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO lifecycle_policy_applications VALUES(?,?,?,?)", id, rev, string(value), time.Now().UnixMilli()); err != nil {
		return "", err
	}
	for i, item := range selections {
		machine, err := readMachine(ctx, tx, item.MachineID)
		missing := errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound)
		if err != nil && !missing {
			return "", err
		}
		var binding *string
		var next *int64
		reason, state := "", "rejected"
		switch {
		case missing || machine.DeletedAt != nil:
			reason = "MACHINE_NOT_FOUND"
		case machine.Revision != item.ExpectedRevision:
			reason = "REVISION_CONFLICT"
		case machine.ActiveOperationID != "":
			reason = "OPERATION_IN_PROGRESS"
		case machine.CurrentBinding == nil || !supported(*machine.CurrentBinding):
			reason = "CAPABILITY_UNSUPPORTED"
		default:
			binding = &machine.CurrentBinding.ID
			p, err := readPolicy(ctx, tx, machine.ID)
			if err != nil {
				return "", err
			}
			if p.AppliedRevision != p.DesiredRevision {
				reason = "POLICY_PENDING"
			} else {
				revision := p.DesiredRevision + 1
				next = &revision
				if _, err = tx.ExecContext(ctx, `UPDATE machine_lifecycle_policies SET desired_revision=?,desired_json=?,overrides_json='{}',source='default_application',
                    default_revision=?,last_operation=?,last_error=NULL WHERE machine_id=?`, revision, string(value), rev, id, machine.ID); err != nil {
					return "", err
				}
				if _, err = tx.ExecContext(ctx, "UPDATE machines SET revision=revision+1 WHERE id=?", machine.ID); err != nil {
					return "", err
				}
				state = "pending"
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_policy_application_targets(application_id,ordinal,machine_id,expected_revision,
            binding_id,policy_revision,state,reason) VALUES(?,?,?,?,?,?,?,nullif(?,''))`, id, i, item.MachineID, item.ExpectedRevision, binding, next, state, reason); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (s *Store) PolicyApplication(ctx context.Context, id string) (PolicyApplication, error) {
	var a PolicyApplication
	var encoded []byte
	err := s.reader.QueryRowContext(ctx, "SELECT id,default_revision,policy_json FROM lifecycle_policy_applications WHERE id=?", id).Scan(&a.ID, &a.DefaultRevision, &encoded)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(encoded, &a.Policy); err != nil {
		return a, err
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT machine_id,expected_revision,policy_revision,state,reason FROM lifecycle_policy_application_targets
        WHERE application_id=? ORDER BY ordinal`, id)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	a.Complete = true
	for rows.Next() {
		var t PolicyApplicationTarget
		if err = rows.Scan(&t.MachineID, &t.ExpectedRevision, &t.PolicyRevision, &t.State, &t.Reason); err != nil {
			return a, err
		}
		if t.State == "pending" {
			a.Complete = false
		}
		a.Targets = append(a.Targets, t)
	}
	return a, rows.Err()
}
