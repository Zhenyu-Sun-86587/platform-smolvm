package platformapi

import (
	"net/http"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
)

type previewSession struct {
	Target, CredentialID, PrincipalID, Host string
	Scope                                   auth.Scope
	Expiry                                  time.Time
}

const previewCookie = "__Host-sandbox-preview"

func (s *Server) previewRoutes() {
	s.route("POST /v1/services/{svc}/preview-ticket", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		v, err := s.getService(r.Context(), r.PathValue("svc"))
		if err != nil {
			return err
		}
		if err = p.Authorize("tunnel", v.MachineID); err != nil {
			return err
		}
		if !v.Enabled || v.PublicHost == "" || r.Header.Get("Origin") != s.Config.API.PublicOrigin {
			return auth.ErrForbidden
		}
		host := v.PublicHost
		switch r.URL.Query().Get("address") {
		case "":
		case "machine":
			host = machineWebHost(v.MachineID, s.Config.Gateway.AppsDomain)
			if host == "" {
				return registry.ErrNotFound
			}
			target, err := s.httpServiceForHost(r.Context(), host)
			if err != nil {
				return err
			}
			if target.ID != v.ID {
				return registry.ErrConflict
			}
		default:
			return registry.ErrInvalid
		}
		key, err := registry.NewID("preview_")
		if err != nil {
			return err
		}
		s.tickets.Store(key, previewSession{Target: v.ID, CredentialID: p.CredentialID, PrincipalID: p.ID, Host: host, Scope: p.Scope, Expiry: time.Now().Add(30 * time.Second)})
		time.AfterFunc(30*time.Second, func() { s.tickets.Delete(key) })
		return write(w, 200, map[string]string{"ticket": key, "action": "https://" + host + "/.sandbox/session"})
	})
}
func (s *Server) previewAuth(w http.ResponseWriter, r *http.Request, v service) (auth.Principal, bool, error) {
	var p auth.Principal
	if r.URL.Path == "/.sandbox/session" {
		if r.Method != "POST" || r.Header.Get("Origin") != s.Config.API.PublicOrigin {
			return p, false, auth.ErrForbidden
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			return p, false, registry.ErrInvalid
		}
		t, ok := s.tickets.LoadAndDelete(r.PostForm.Get("ticket"))
		if !ok {
			return p, false, auth.ErrUnauthenticated
		}
		session, ok := t.(previewSession)
		if !ok || session.Target != v.ID || session.Host != requestHostname(r.Host) || !session.Expiry.After(time.Now()) {
			return p, false, auth.ErrForbidden
		}
		key, err := registry.NewID("service_")
		if err != nil {
			return p, false, err
		}
		session.Expiry = time.Now().Add(30 * time.Minute)
		s.tickets.Store(key, session)
		time.AfterFunc(30*time.Minute, func() { s.tickets.Delete(key) })
		http.SetCookie(w, &http.Cookie{Name: previewCookie, Value: key, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 1800})
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/", 303)
		return p, true, nil
	}
	if c, err := r.Cookie(previewCookie); err == nil {
		stored, ok := s.tickets.Load(c.Value)
		if !ok {
			return p, false, auth.ErrUnauthenticated
		}
		session, ok := stored.(previewSession)
		if !ok || session.Target != v.ID || session.Host != requestHostname(r.Host) || !session.Expiry.After(time.Now()) {
			return p, false, auth.ErrForbidden
		}
		// The preview cookie is confined to the selected service and host above.
		// AccessAt revalidates the underlying credential's exact original scope.
		principal := auth.Principal{ID: session.PrincipalID, CredentialID: session.CredentialID, Scope: session.Scope}
		if err := s.CheckAccessPrincipal(r.Context(), principal, v.MachineID); err != nil {
			return p, false, err
		}
		return principal, false, principal.Authorize(auth.ActionTunnel, v.MachineID)
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		p, err := s.Auth.Authenticate(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		return p, false, err
	}
	return p, false, auth.ErrUnauthenticated
}
