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
	rt "sandbox-platform/internal/runtime"
)

func guestContext(binding registry.Binding) auth.AttestedGuestContext {
	return auth.AttestedGuestContext{
		MachineID: binding.MachineID, BindingID: binding.ID, RuntimeInstanceID: binding.RuntimeInstanceID,
		RuntimeBundleID: binding.RuntimeBundleID, RuntimeObjectID: binding.RuntimeInstanceObjectID,
		IncarnationID: binding.IncarnationID, ExecutionEpoch: binding.ExecutionEpoch,
	}
}

func (s *Server) guestGrantRoutes() {
	s.route("POST /v1/guest-grants", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		var grant auth.GuestGrantSpec
		if err := decode(r, &grant); err != nil {
			return err
		}
		if grant.Action == auth.GuestProxy && s.Config.CredentialProxy(grant.ProxyID) == nil {
			return registry.ErrInvalid
		}
		return s.mutate(w, r, p, grant, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			id, err := s.Auth.CreateGuestGrantTx(ctx, tx, p, grant)
			return registry.MutationResult{Response: lifecycle.JSON(map[string]any{"grant_id": id, "revision": 1})}, err
		})
	})
	s.route("GET /v1/guest-grants", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		rows, err := s.DB.ReadDB().QueryContext(r.Context(), `SELECT g.id,g.source_machine_id,g.action,COALESCE(g.target_machine_id,''),g.expires_at,g.maximum_uses,g.used_count,g.revision,g.revoked_at,
			COALESCE(g.proxy_id,''),g.maximum_concurrent,g.limits_json
			FROM guest_grants g JOIN guest_identities i ON i.machine_id=g.source_machine_id WHERE i.owner_principal_id=? ORDER BY g.id`, p.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		type item struct {
			auth.GuestGrantSpec
			ID        string     `json:"grant_id"`
			Uses      int64      `json:"used_count"`
			Revision  int64      `json:"revision"`
			RevokedAt *time.Time `json:"revoked_at,omitempty"`
		}
		items := []item{}
		for rows.Next() {
			var v item
			var expires int64
			var revoked sql.NullInt64
			var limits string
			if err := rows.Scan(&v.ID, &v.SourceMachineID, &v.Action, &v.TargetMachineID, &expires, &v.MaximumUses, &v.Uses, &v.Revision, &revoked,
				&v.ProxyID, &v.MaximumConcurrent, &limits); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(limits), &v.Limits); err != nil {
				return err
			}
			v.ExpiresAt = time.UnixMilli(expires).UTC()
			if revoked.Valid {
				at := time.UnixMilli(revoked.Int64).UTC()
				v.RevokedAt = &at
			}
			items = append(items, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return list(w, r, items, func(v item) string { return v.ID })
	})
	s.route("DELETE /v1/guest-grants/{grant}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		rev, err := revision(r)
		if err != nil {
			return err
		}
		var body struct{}
		if err := decode(r, &body); err != nil {
			return err
		}
		id := r.PathValue("grant")
		return s.mutate(w, r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			err := s.Auth.RevokeGuestGrantTx(ctx, tx, p, id, rev)
			return registry.MutationResult{Response: lifecycle.JSON(map[string]any{"grant_id": id, "status": "revoked", "revision": rev + 1})}, err
		})
	})
	s.route("POST /v1/guest-grants/{grant}/credential", s.issueGuestCredential)
}

func (s *Server) issueGuestCredential(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
		return err
	}
	var body struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.TTLSeconds < 1 || body.TTLSeconds > 600 {
		return registry.ErrInvalid
	}
	ctx := r.Context()
	id := r.PathValue("grant")
	var source string
	if err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT g.source_machine_id FROM guest_grants g JOIN guest_identities i ON i.machine_id=g.source_machine_id
		WHERE g.id=? AND i.owner_principal_id=?`, id, p.ID).Scan(&source); err != nil {
		return err
	}
	// Resolve the exact published execution from a host-trusted binding and
	// confirm its live Worker identity before issuing credentials for it.
	machine, err := s.DB.GetMachine(ctx, source)
	if err != nil {
		return err
	}
	binding := machine.CurrentBinding
	if binding == nil || binding.PowerState != registry.PowerRunning || binding.ExecutionEpoch < 1 || machine.DeletedAt != nil {
		return registry.ErrStaleObservation
	}
	adapter, err := s.ForBinding(*binding)
	if err != nil {
		return err
	}
	control, ok := adapter.(rt.NativeControl)
	if !ok {
		return rt.ErrUnsupported
	}
	ref := binding.ExecutionRef()
	observation, err := control.NativeInventory(ctx, ref)
	if err != nil {
		return err
	}
	if observation.Execution.ExecutionRef != ref || observation.Execution.Phase != "registered" || observation.Inventory.SourceDisposition != "RUNNING" {
		return registry.ErrStaleObservation
	}
	w.Header().Set("Cache-Control", "no-store")
	return s.mutate(w, r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		secret, err := s.Auth.IssueGuestCredentialTx(ctx, tx, p, guestContext(*binding), id, time.Duration(body.TTLSeconds)*time.Second)
		if err != nil {
			return registry.MutationResult{}, err
		}
		return registry.MutationResult{
			Response:      lifecycle.JSON(map[string]any{"grant_id": id, "status": "issued", "token": secret}),
			RetryResponse: lifecycle.JSON(map[string]any{"grant_id": id, "status": "issued"}),
		}, nil
	})
}
