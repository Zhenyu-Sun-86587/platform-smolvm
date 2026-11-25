package platformapi

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) operationsRoutes() {
	for _, act := range []string{"reconcile", "cancel"} {
		s.route("POST /v1/operations/{op}/"+act, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			id := r.PathValue("op")
			op, err := s.DB.GetOperation(r.Context(), id)
			if err != nil {
				return err
			}
			if err = p.Authorize("manage", op.MachineID); err != nil {
				return err
			}
			var body struct{}
			if err = decode(r, &body); err != nil {
				return err
			}
			return s.mutate(w, r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
				step := registry.TransitionRequest{State: registry.Observing, Stage: "reconciling"}
				if act == "reconcile" && op.State != registry.RecoveryRequired {
					return registry.MutationResult{}, registry.ErrOperationState
				}
				if act == "cancel" {
					if op.Kind == "wake" {
						return registry.MutationResult{}, &lifecycle.Failure{Code: "OPERATION_IN_PROGRESS", Message: "shared wake remains owned by lifecycle worker"}
					}
					step = registry.TransitionRequest{State: registry.Cancelled, Stage: "completed", Effect: "none"}
				}
				step.ExpectedVersion = op.Version
				updated, err := s.DB.TransitionTx(ctx, tx, id, step)
				if err != nil {
					return registry.MutationResult{}, err
				}
				return registry.MutationResult{Response: lifecycle.JSON(updated)}, nil
			})
		})
	}
	for route, kind := range map[string]string{"GET /v1/operations": "", "GET /v1/restores": "restore"} {
		s.route(route, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			rows, err := s.DB.ReadDB().QueryContext(r.Context(), "SELECT id,COALESCE(machine_id,'') FROM operations WHERE (?='' OR kind IN (?,?)) ORDER BY created_at DESC,id", kind, kind, rt.VMRestore)
			if err != nil {
				return err
			}
			var ids []string
			for rows.Next() {
				var id, mid string
				if err = rows.Scan(&id, &mid); err != nil {
					rows.Close()
					return err
				}
				if p.Allows("read", mid) {
					ids = append(ids, id)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			items := make([]registry.Operation, 0, len(ids))
			for _, id := range ids {
				op, err := s.DB.GetOperation(r.Context(), id)
				if err != nil {
					return err
				}
				items = append(items, op)
			}
			return list(w, r, items, func(op registry.Operation) string { return op.ID })
		})
	}
	s.route("GET /v1/machines/{machine}/dependencies", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		if _, err := s.DB.GetMachine(r.Context(), id); err != nil {
			return err
		}
		rows, err := s.DB.ReadDB().QueryContext(r.Context(), `SELECT a.machine_id,b.machine_id,d.kind,d.status,d.observed_at FROM dependencies d JOIN runtime_bindings a ON a.id=d.parent_binding_id JOIN runtime_bindings b ON b.id=d.child_binding_id WHERE a.machine_id=? OR b.machine_id=? ORDER BY a.machine_id,b.machine_id,d.kind`, id, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var a, b, kind, status string
			var at sql.NullInt64
			if err := rows.Scan(&a, &b, &kind, &status, &at); err != nil {
				return err
			}
			row := map[string]any{"parent_machine_id": a, "child_machine_id": b, "kind": kind, "status": status}
			if at.Valid {
				row["observed_at"] = time.UnixMilli(at.Int64).UTC()
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return write(w, 200, map[string]any{"items": out})
	})
}
