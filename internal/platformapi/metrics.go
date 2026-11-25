package platformapi

import (
	"context"
	"net/http"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type platformMetrics struct {
	AccessHolds int            `json:"active_access_holds"`
	WakeWaiters int            `json:"wake_waiters"`
	Operations  map[string]int `json:"operations_by_state"`
}

func (s *Server) platformMetrics(ctx context.Context, machine string) (platformMetrics, error) {
	holds, waiters := s.AccessCounts(machine)
	v := platformMetrics{AccessHolds: holds, WakeWaiters: waiters, Operations: map[string]int{}}
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT state, COUNT(*) FROM operations WHERE (?='' OR machine_id=?) GROUP BY state", machine, machine)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err = rows.Scan(&state, &count); err != nil {
			return v, err
		}
		v.Operations[state] = count
	}
	return v, rows.Err()
}

func (s *Server) metricsRoutes() {
	s.route("GET /v1/machines/{machine}/metrics", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(r.Context(), id)
		if err != nil {
			return err
		}
		if m.CurrentBinding == nil {
			return registry.ErrGeneration
		}
		native, err := s.Runtime.Metrics(r.Context(), m.CurrentBinding.RuntimeName)
		if err != nil {
			return err
		}
		platform, err := s.platformMetrics(r.Context(), id)
		if err != nil {
			return err
		}
		return write(w, 200, struct {
			rt.Metrics
			Platform platformMetrics `json:"platform"`
		}{native, platform})
	})
	s.route("GET /v1/system/metrics", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("read", ""); err != nil {
			return err
		}
		native, err := s.Runtime.Capacity(r.Context())
		if err != nil {
			return err
		}
		platform, err := s.platformMetrics(r.Context(), "")
		if err != nil {
			return err
		}
		return write(w, 200, struct {
			rt.Capacity
			Platform platformMetrics `json:"platform"`
		}{native, platform})
	})
}
