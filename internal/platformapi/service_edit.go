package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type readiness struct {
	Type   string `json:"type,omitempty"`
	Path   string `json:"path,omitempty"`
	Status int    `json:"expected_status,omitempty"`
}
type accessPolicy struct {
	Private bool `json:"private"`
}
type serviceChange struct {
	Edit     serviceEdit `json:"edit"`
	ID       string      `json:"service_id"`
	Revision int64       `json:"revision"`
}

func (v serviceEdit) validate() error {
	if len(v.Name) > 100 {
		return registry.ErrInvalid
	}
	if v.Auth != nil && !v.Auth.Private {
		return registry.ErrInvalid
	}
	if r := v.Readiness; r != nil {
		switch r.Type {
		case "tcp", "":
			if r.Path != "" || r.Status != 0 {
				return registry.ErrInvalid
			}
		case "http":
			u, err := url.ParseRequestURI(r.Path)
			if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") || r.Status < 100 || r.Status > 599 {
				return registry.ErrInvalid
			}
		default:
			return registry.ErrInvalid
		}
	}
	return nil
}
func (s *Server) servicePatchRoute() {
	s.Extra["service_update"] = s.updateService
	s.route("PATCH /v1/services/{svc}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		rev, err := revision(r)
		if err != nil {
			return err
		}
		var edit serviceEdit
		if err = decode(r, &edit); err != nil {
			return err
		}
		if err = edit.validate(); err != nil {
			return err
		}
		id := r.PathValue("svc")
		return s.acceptServiceChange(w, r, p, id, rev, "service_update", serviceChange{edit, id, rev})
	})
}

// acceptServiceChange checks the service revision and takes its machine lock in
// the same transaction. A replay is authorized against the saved operation.
func (s *Server) acceptServiceChange(w http.ResponseWriter, r *http.Request, p auth.Principal, id string, rev int64, kind string, request any) error {
	return s.accept(w, r, p, request, registry.AcceptRequest{
		Kind: kind,
		ResolveMachine: func(ctx context.Context, tx *sql.Tx, originalMachine string) (string, error) {
			if originalMachine != "" {
				return originalMachine, p.Authorize(auth.ActionManage, originalMachine)
			}
			var mid string
			var actual int64
			if err := tx.QueryRowContext(ctx, "SELECT machine_id, revision FROM services WHERE id=?", id).Scan(&mid, &actual); err != nil {
				return "", err
			}
			if err := p.Authorize(auth.ActionManage, mid); err != nil {
				return "", err
			}
			if rev != actual {
				return "", registry.ErrRevision
			}
			return mid, nil
		},
	})
}

func (s *Server) updateService(ctx context.Context, j *lifecycle.Job) error {
	var req serviceChange
	if err := json.Unmarshal(j.Op.Request, &req); err != nil {
		return err
	}
	v, err := s.getService(ctx, req.ID)
	if err != nil {
		return err
	}
	if v.MachineID != j.Op.MachineID || v.Revision != req.Revision {
		return registry.ErrRevision
	}
	if req.Edit.Protocol != "" && req.Edit.Protocol != v.Protocol {
		return &lifecycle.Failure{Code: "PROFILE_UNSUPPORTED", Message: "a different ingress protocol requires a new service"}
	}
	if req.Edit.Name != "" {
		v.Name = req.Edit.Name
	}
	if req.Edit.Readiness != nil {
		v.Readiness = *req.Edit.Readiness
	}
	if req.Edit.GuestPort != 0 && req.Edit.GuestPort != v.GuestPort {
		var port uint16
		if err = s.DB.ReadDB().QueryRowContext(ctx, "SELECT host_port FROM port_allocations WHERE service_id=? AND binding_id=? AND state='bound'", v.ID, j.Binding.ID).Scan(&port); err != nil {
			return err
		}
		o, err := s.Runtime.Observe(ctx, j.Binding.RuntimeName)
		if err != nil {
			return err
		}
		if o.Labels["platform.binding"] != j.Binding.ID {
			return registry.ErrStaleObservation
		}
		found := false
		for i := range o.Ports {
			if o.Ports[i].Guest == req.Edit.GuestPort {
				return registry.ErrConflict
			}
			if o.Ports[i].Host == port && o.Ports[i].Guest == v.GuestPort {
				found = true
				o.Ports[i].Guest = req.Edit.GuestPort
			}
		}
		if !found {
			return errors.New("current native port mapping unavailable")
		}
		if err = j.Stage(ctx, "set_ports", ""); err != nil {
			return err
		}
		if _, err = s.Runtime.SetPorts(ctx, j.Binding.RuntimeName, o.Ports); err != nil {
			return err
		}
		j.Written = true
		latest, err := s.Runtime.Observe(ctx, j.Binding.RuntimeName)
		if err != nil {
			return err
		}
		found = false
		for _, p := range latest.Ports {
			if p == (rt.Port{Host: port, Guest: req.Edit.GuestPort, Protocol: "tcp"}) {
				found = true
			}
		}
		if !found {
			return errors.New("changed port mapping unverified")
		}
		v.GuestPort = req.Edit.GuestPort
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]any{"service_id": v.ID, "revision": v.Revision + 1}), Apply: func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE services SET name=?,guest_port=?,readiness_json=?,revision=revision+1 WHERE id=? AND revision=?", v.Name, v.GuestPort, string(lifecycle.JSON(v.Readiness)), v.ID, v.Revision)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return registry.ErrRevision
		}
		_, err = tx.ExecContext(ctx, "UPDATE port_allocations SET guest_port=? WHERE service_id=? AND binding_id=? AND state='bound'", v.GuestPort, v.ID, j.Binding.ID)
		return err
	}})
}
func probeHTTP(ctx context.Context, b gateway.Backend, r readiness) error {
	if r.Type != "http" {
		return nil
	}
	target := b.URL() + r.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != r.Status {
		return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "HTTP readiness returned unexpected status"}
	}
	return nil
}
