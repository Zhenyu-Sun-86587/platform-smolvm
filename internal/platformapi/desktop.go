package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/templates"
)

type desktopSession struct {
	ID         string    `json:"desktop_session_id"`
	MachineID  string    `json:"machine_id"`
	ServiceID  string    `json:"service_id"`
	LeaseID    string    `json:"lease_id"`
	BindingID  string    `json:"binding_id"`
	Generation int64     `json:"binding_generation"`
	Epoch      int64     `json:"execution_epoch"`
	State      string    `json:"state"`
	Revision   int64     `json:"revision"`
	Expires    time.Time `json:"expires_at"`
	leaseOwner string
	credential string
	serviceRev int64
	workerRev  sql.NullInt64
}

const desktopSessions = `SELECT d.id,l.machine_id,d.service_id,d.lease_id,d.binding_id,l.binding_generation,
	d.execution_epoch,l.state,l.revision,l.expires_at,l.principal_id,d.credential_id,d.service_revision,l.worker_revision
	FROM desktop_sessions d JOIN leases l ON l.id=d.lease_id`

func readDesktopSession(row row) (desktopSession, error) {
	var session desktopSession
	var expires int64
	err := row.Scan(&session.ID, &session.MachineID, &session.ServiceID, &session.LeaseID, &session.BindingID,
		&session.Generation, &session.Epoch, &session.State, &session.Revision, &expires,
		&session.leaseOwner, &session.credential, &session.serviceRev, &session.workerRev)
	session.Expires = time.UnixMilli(expires).UTC()
	return session, err
}

func (s *Server) desktopRoutes() {
	s.route("POST /v1/machines/{machine}/desktop-sessions", s.createDesktopSession)
	s.route("GET /v1/machines/{machine}/desktop-sessions", s.listDesktopSessions)
	s.route("GET /v1/desktop-sessions/{session}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		session, err := s.desktopFor(r.Context(), r.PathValue("session"), p)
		if err != nil {
			return err
		}
		return write(w, 200, session)
	})
	s.route("DELETE /v1/desktop-sessions/{session}", s.closeDesktopSession)
	s.route("POST /v1/desktop-sessions/{session}/tunnel-tickets", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("session")
		if _, err := s.admitDesktop(r.Context(), id, p); err != nil {
			return err
		}
		return s.issueTicket(w, r, id)
	})
	s.mux.HandleFunc("GET /v1/desktop-sessions/{session}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.desktopTunnel(w, r); err != nil {
			fail(w, r, err)
		}
	})
}

func (s *Server) createDesktopSession(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	mid := r.PathValue("machine")
	if err := p.Authorize(auth.ActionTunnel, mid); err != nil {
		return err
	}
	if p.CredentialID == "" {
		return auth.ErrUnauthenticated
	}
	var request struct {
		ServiceID  string `json:"service_id"`
		Generation int64  `json:"expected_generation"`
		TTL        int64  `json:"ttl_seconds,omitempty"`
	}
	if err := decode(r, &request); err != nil {
		return err
	}
	if request.TTL == 0 {
		request.TTL = 900
	}
	if request.ServiceID == "" || request.Generation < 1 || request.TTL < 1 || request.TTL > 86400 {
		return registry.ErrInvalid
	}
	rev, err := revision(r)
	if err != nil {
		return err
	}
	raw, err := s.applyMutation(r, p, request, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		var session desktopSession
		var binding registry.Binding
		var machineRev int64
		var active bool
		var desktopProfile, controlEpoch string
		if err := tx.QueryRowContext(ctx, `SELECT b.id,b.generation,COALESCE(b.execution_epoch,0),m.revision,
			COALESCE(json_extract(t.spec_json,'$.desktop_profile'),''),ctl.control_epoch,COALESCE(b.runtime_instance_id,''),
			COALESCE(b.runtime_bundle_id,''),COALESCE(b.runtime_instance_object_id,''),COALESCE(b.incarnation_id,''),b.power_state,
			EXISTS(SELECT 1 FROM operation_locks WHERE machine_id=m.id)
			FROM machines m JOIN templates t ON t.id=m.template_id JOIN runtime_bindings b ON b.id=m.current_binding_id
			JOIN meta ctl ON ctl.id=1 WHERE m.id=? AND m.deleted_at IS NULL`, mid).
			Scan(&session.BindingID, &session.Generation, &session.Epoch, &machineRev, &desktopProfile, &controlEpoch,
				&binding.RuntimeInstanceID, &binding.RuntimeBundleID, &binding.RuntimeInstanceObjectID, &binding.IncarnationID, &binding.PowerState, &active); err != nil {
			return registry.MutationResult{}, err
		}
		if desktopProfile != templates.DesktopSoftwareVNC || session.Epoch < 1 {
			return registry.MutationResult{}, &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "desktop sessions require the System software VNC profile and an execution"}
		}
		if machineRev != rev {
			return registry.MutationResult{}, registry.ErrRevision
		}
		if session.Generation != request.Generation {
			return registry.MutationResult{}, registry.ErrGeneration
		}
		if active {
			return registry.MutationResult{}, registry.ErrOperationActive
		}
		binding.ID, binding.Generation, binding.ExecutionEpoch = session.BindingID, session.Generation, session.Epoch
		_, managed, err := s.desktopWorker(binding)
		if err != nil {
			return registry.MutationResult{}, err
		}
		if managed && (binding.PowerState == registry.PowerHibernated || binding.PowerState == registry.PowerStopped) {
			return registry.MutationResult{}, desktopExecutionRequired()
		}
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM services WHERE id=? AND machine_id=? AND enabled=1
			AND kind='vnc' AND public_host IS NULL AND public_port IS NULL`, request.ServiceID, mid).Scan(&session.serviceRev); err != nil {
			return registry.MutationResult{}, err
		}
		if session.ID, err = registry.NewID("desk_"); err != nil {
			return registry.MutationResult{}, err
		}
		if session.LeaseID, err = registry.NewID("lease_"); err != nil {
			return registry.MutationResult{}, err
		}
		secret, err := registry.NewID("desktop-lease-")
		if err != nil {
			return registry.MutationResult{}, err
		}
		now := time.Now().UTC()
		session.MachineID, session.ServiceID = mid, request.ServiceID
		session.State, session.Revision, session.Expires = "active", 1, now.Add(time.Duration(request.TTL)*time.Second)
		semantics := 1
		if managed {
			session.State, semantics = "suspect", 2
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases(id,machine_id,binding_generation,principal_id,token_hash,reason,expires_at,state,semantics_version)
			VALUES(?,?,?,?,?,'Desktop session',?,?,?)`, session.LeaseID, mid, session.Generation, p.ID,
			fmt.Sprintf("%x", sha256.Sum256([]byte(secret))), session.Expires.UnixMilli(), session.State, semantics); err != nil {
			return registry.MutationResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO desktop_sessions VALUES (?,?,?,?,?,?,?,?,?)`, session.ID, session.LeaseID,
			session.ServiceID, session.serviceRev, session.BindingID, session.Epoch, p.CredentialID, controlEpoch, now.UnixMilli()); err != nil {
			return registry.MutationResult{}, err
		}
		if err := registry.AppendEvent(ctx, tx, "lease", mid, "", map[string]string{"lease_id": session.LeaseID, "state": session.State}, false); err != nil {
			return registry.MutationResult{}, err
		}
		return registry.MutationResult{Response: lifecycle.JSON(session)}, nil
	})
	if err != nil {
		return err
	}
	var saved struct {
		ID string `json:"desktop_session_id"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	session, err := s.admitDesktop(r.Context(), saved.ID, p)
	if err != nil {
		return err
	}
	return write(w, http.StatusOK, session)
}

func (s *Server) requiresDesktopSession(ctx context.Context, v service) (bool, error) {
	if v.Protocol != "vnc" {
		return false, nil
	}
	var enabled bool
	err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM machines m JOIN templates t ON t.id=m.template_id
		WHERE m.id=? AND m.deleted_at IS NULL AND json_extract(t.spec_json,'$.desktop_profile')=?)`, v.MachineID, templates.DesktopSoftwareVNC).Scan(&enabled)
	return enabled, err
}

func (s *Server) desktopFor(ctx context.Context, id string, p auth.Principal) (desktopSession, error) {
	session, err := readDesktopSession(s.DB.ReadDB().QueryRowContext(ctx, desktopSessions+" WHERE d.id=?", id))
	if err != nil {
		return session, err
	}
	if session.leaseOwner != p.ID || session.credential != p.CredentialID {
		return desktopSession{}, auth.ErrForbidden
	}
	return session, p.Authorize(auth.ActionTunnel, session.MachineID)
}

func (s *Server) listDesktopSessions(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	mid := r.PathValue("machine")
	if err := p.Authorize(auth.ActionTunnel, mid); err != nil {
		return err
	}
	rows, err := s.DB.ReadDB().QueryContext(r.Context(), desktopSessions+" WHERE l.machine_id=? AND l.principal_id=? AND d.credential_id=? ORDER BY d.created_at DESC,d.id", mid, p.ID, p.CredentialID)
	if err != nil {
		return err
	}
	defer rows.Close()
	results := []desktopSession{}
	for rows.Next() {
		session, err := readDesktopSession(rows)
		if err != nil {
			return err
		}
		results = append(results, session)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return list(w, r, results, func(session desktopSession) string { return session.ID })
}

func (s *Server) closeDesktopSession(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	session, err := s.desktopFor(r.Context(), r.PathValue("session"), p)
	if err != nil {
		return err
	}
	var body struct{}
	if err := decode(r, &body); err != nil {
		return err
	}
	raw, err := s.applyMutation(r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		current, err := readDesktopSession(tx.QueryRowContext(ctx, desktopSessions+" WHERE d.id=?", session.ID))
		if err != nil {
			return registry.MutationResult{}, err
		}
		if current.State != "released" {
			if _, err := tx.ExecContext(ctx, "UPDATE leases SET state='released',revision=revision+1 WHERE id=?", current.LeaseID); err != nil {
				return registry.MutationResult{}, err
			}
			if err := registry.AppendEvent(ctx, tx, "lease", current.MachineID, "", map[string]string{"lease_id": current.LeaseID, "state": "released"}, false); err != nil {
				return registry.MutationResult{}, err
			}
			current.State = "released"
			current.Revision++
		}
		return registry.MutationResult{Response: lifecycle.JSON(current)}, nil
	})
	if err != nil {
		return err
	}
	if err := s.releaseDesktopProtection(r.Context(), session); err != nil {
		return err
	}
	return write(w, http.StatusOK, raw)
}

type desktopReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func desktopValid(ctx context.Context, db desktopReader, id string) error {
	var valid bool
	now := time.Now().UnixMilli()
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM desktop_sessions d
		JOIN leases l ON l.id=d.lease_id JOIN machines w ON w.id=l.machine_id AND w.current_binding_id=d.binding_id
		JOIN runtime_bindings b ON b.id=d.binding_id AND b.execution_epoch=d.execution_epoch AND b.generation=l.binding_generation
		JOIN services svc ON svc.id=d.service_id AND svc.revision=d.service_revision AND svc.enabled=1
			AND svc.kind='vnc' AND svc.public_host IS NULL AND svc.public_port IS NULL
		JOIN credentials c ON c.id=d.credential_id AND c.principal_id=l.principal_id AND c.revoked_at IS NULL
		JOIN principals owner ON owner.id=l.principal_id AND owner.disabled=0 JOIN meta ctl ON ctl.id=1 AND ctl.control_epoch=d.control_epoch
		WHERE d.id=? AND w.deleted_at IS NULL AND l.state!='released' AND l.expires_at>? AND (c.expires_at IS NULL OR c.expires_at>?)
		AND ctl.maintenance=0)`, id, now, now).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return &lifecycle.Failure{Code: "DESKTOP_SESSION_ENDED", Message: "desktop session binding, authorization or service changed"}
	}
	return nil
}

func (s *Server) verifyDesktop(ctx context.Context, id string, p auth.Principal) error {
	session, err := s.desktopFor(ctx, id, p)
	if err != nil {
		return err
	}
	if session.State != "active" {
		return &lifecycle.Failure{Code: "DESKTOP_SESSION_ENDED", Message: "desktop session has no active lease"}
	}
	return desktopValid(ctx, s.DB.ReadDB(), id)
}

func (s *Server) desktopTunnel(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("session")
	p, err := s.streamPrincipal(r, id)
	if err != nil {
		return err
	}
	session, err := s.admitDesktop(r.Context(), id, p)
	if err != nil {
		return err
	}
	v, err := s.getService(r.Context(), session.ServiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.tunnelAuthorized(w, r, v, p, func(ctx context.Context) error { return s.verifyDesktop(ctx, id, p) })
}
