package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

func (s *Server) policyApplicationRoutes() {
	s.route("GET /v1/machines/{machine}/lifecycle", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(r.Context(), id)
		if err != nil {
			return err
		}
		v, err := s.lifecycleView(r.Context(), m, true)
		if err != nil {
			return err
		}
		return write(w, 200, v)
	})
	s.route("GET /v1/lifecycle-policy-applications/{application}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		result, err := s.DB.PolicyApplication(r.Context(), r.PathValue("application"))
		if err != nil {
			return err
		}
		return write(w, http.StatusOK, result)
	})
	s.route("POST /v1/lifecycle-policy-applications", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		var body struct {
			Revision int64                      `json:"default_policy_revision"`
			Machines []registry.PolicySelection `json:"machines"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if body.Revision < 1 || len(body.Machines) == 0 || len(body.Machines) > 256 {
			return registry.ErrInvalid
		}
		seen := map[string]bool{}
		for _, m := range body.Machines {
			if m.MachineID == "" || m.ExpectedRevision < 1 || seen[m.MachineID] {
				return registry.ErrInvalid
			}
			seen[m.MachineID] = true
		}
		raw, err := s.applyMutation(r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			id, err := registry.ReservePolicyApplication(ctx, tx, body.Revision, body.Machines, func(b registry.Binding) bool {
				a, err := s.ForBinding(b)
				if err != nil {
					return false
				}
				_, ok := lifecycle.PolicyClient(a, b)
				return ok
			})
			return registry.MutationResult{Response: lifecycle.JSON(id)}, err
		})
		if err != nil {
			return err
		}
		var id string
		if err = json.Unmarshal(raw, &id); err != nil {
			return err
		}
		a, err := s.DB.PolicyApplication(r.Context(), id)
		if err != nil {
			return err
		}
		// Finish immediately for healthy local Workers. The existing monitor
		// replays each durable delivery if this HTTP request ends first.
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		for _, target := range a.Targets {
			if ctx.Err() != nil {
				break
			}
			if target.State != "pending" {
				continue
			}
			m, err := s.DB.GetMachine(ctx, target.MachineID)
			if err != nil || m.CurrentBinding == nil {
				continue
			}
			adapter, err := s.ForBinding(*m.CurrentBinding)
			if err != nil {
				continue
			}
			if c, ok := lifecycle.PolicyClient(adapter, *m.CurrentBinding); ok {
				_, _ = s.Engine.ApplyMachinePolicy(ctx, *m.CurrentBinding, c)
			}
		}
		a, err = s.DB.PolicyApplication(r.Context(), id)
		if err != nil {
			return err
		}
		status := http.StatusOK
		if !a.Complete {
			status = http.StatusAccepted
		}
		w.Header().Set("Location", "/v1/lifecycle-policy-applications/"+id)
		return write(w, status, a)
	})
}

func (s *Server) lifecycleView(ctx context.Context, m registry.Machine, refresh bool) (registry.LifecycleSettings, error) {
	v, err := s.DB.LifecyclePolicy(ctx, m.ID)
	b := m.CurrentBinding
	if err != nil || b == nil || b.RuntimeBundleID == "" {
		return v, err
	}
	a, err := s.ForBinding(*b)
	if err != nil {
		if refresh {
			v.Worker = nil
		}
		projectPolicyStatus(&v, false)
		return v, nil
	}
	client, supported := lifecycle.PolicyClient(a, *b)
	observed := !refresh && len(v.Worker) > 0
	if refresh && supported && v.AppliedRevision == v.DesiredRevision {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		state, err := s.Engine.ObservePolicy(probe, *b, client)
		cancel()
		savedOrStopped := state.Incarnation == "" && (state.Power == "HIBERNATED" || state.Power == "STOPPED")
		observed = err == nil && state.Epoch == b.ExecutionEpoch && state.Applied == v.AppliedRevision &&
			(state.Incarnation == b.IncarnationID || savedOrStopped)
		if observed {
			v, err = s.DB.LifecyclePolicy(ctx, m.ID)
			if err != nil {
				return v, err
			}
			observed = v.BindingID == b.ID && state.Applied == v.AppliedRevision
			if observed {
				v.Worker = state.Observation
			}
		}
	}
	if refresh && (!supported || !observed) {
		v.Worker = nil
	}
	v.Supported = supported
	projectPolicyStatus(&v, observed)
	return v, nil
}

func projectPolicyStatus(policy *registry.LifecycleSettings, observed bool) {
	policy.EffectiveBlockers = nil
	switch {
	case !policy.Supported:
		policy.EffectiveStatus = "blocked"
		policy.EffectiveBlockers = []string{"runtime_profile_unsupported"}
	case policy.AppliedRevision != policy.DesiredRevision:
		policy.EffectiveStatus = "pending"
	case !policy.Desired.AutoStandby && !policy.Desired.AutoHibernate:
		policy.EffectiveStatus = "disabled"
	default:
		var status lifecycle.WorkerLifecycleStatus
		if !observed || len(policy.Worker) == 0 || json.Unmarshal(policy.Worker, &status) != nil || status.Power == "" {
			policy.EffectiveStatus = "blocked"
			policy.EffectiveBlockers = []string{"unknown:worker"}
			return
		}
		policy.EffectiveStatus = "applied"
		if status.Power != "HIBERNATED" && status.Power != "STOPPED" && len(status.Blockers) != 0 {
			policy.EffectiveStatus = "blocked"
			policy.EffectiveBlockers = status.Blockers
		}
	}
}
