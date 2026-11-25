package platformapi

import (
	"context"
	"database/sql"
	"net/http"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

func (s *Server) backupRoutes() {
	s.route("GET /v1/backups/policies", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		return list(w, r, s.Config.Backups, func(policy config.BackupPolicy) string { return policy.ID })
	})
	s.route("GET /v1/backups", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		runs, err := s.DB.Backups(r.Context(), r.URL.Query().Get("policy_id"))
		if err != nil {
			return err
		}
		return list(w, r, runs, func(run registry.BackupRun) string { return run.ID })
	})
	s.route("GET /v1/backups/{backup}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		run, err := s.DB.GetBackup(r.Context(), r.PathValue("backup"))
		if err != nil {
			return err
		}
		return write(w, 200, run)
	})
	s.route("POST /v1/backups/policies/{policy}/runs", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		var body struct{}
		if err := decode(r, &body); err != nil {
			return err
		}
		raw, err := s.applyMutation(r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			for _, policy := range s.Config.Backups {
				if policy.ID == r.PathValue("policy") {
					run, err := s.DB.AcceptBackupTx(ctx, tx, policy.ID, policy.Scope)
					return registry.MutationResult{Response: lifecycle.JSON(run)}, err
				}
			}
			return registry.MutationResult{}, registry.ErrNotFound
		})
		if err != nil {
			return err
		}
		return write(w, 202, raw)
	})
}
