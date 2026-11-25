// Package platformapi serves the authenticated management plane.
package platformapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/buildinfo"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

type Server struct {
	*lifecycle.Engine
	Snapshots   *snapshots.Catalog
	Auth        *auth.Service
	Web         http.Handler
	mux         *http.ServeMux
	loginSlots  chan struct{}
	tickets     sync.Map
	execStreams sync.Map
	uploadSlots chan struct{}
}

func New(e *lifecycle.Engine, catalog *snapshots.Catalog, web http.Handler) *Server {
	s := &Server{Engine: e, Snapshots: catalog, Auth: auth.New(e.DB), Web: web, mux: http.NewServeMux(), loginSlots: make(chan struct{}, 4), uploadSlots: make(chan struct{}, 8)}
	s.routes()
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if n, _, e := net.SplitHostPort(host); e == nil {
		host = n
	}
	if r.TLS != nil && r.TLS.ServerName != "" && !strings.EqualFold(r.TLS.ServerName, host) {
		w.Header().Set("X-Sandbox-Forwarding", "not_started")
		http.Error(w, "TLS connection belongs to another host", http.StatusMisdirectedRequest)
		return
	}
	if s.Config.Gateway.AppsDomain != "" && strings.HasSuffix(host, "."+s.Config.Gateway.AppsDomain) {
		s.serviceHTTP(w, r, host)
		return
	}
	if !strings.EqualFold(r.Host, s.ManagementHost()) {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/v1" {
		s.Web.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

type handler func(http.ResponseWriter, *http.Request, auth.Principal) error

func (s *Server) route(path string, h handler) {
	s.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		p, secret, err := s.authenticate(r)
		if err == nil && secret != "" && r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != s.Config.API.PublicOrigin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf(secret))) != 1 {
				err = auth.ErrForbidden
			}
		}
		if err == nil {
			err = h(w, r, p)
		}
		if err != nil {
			fail(w, r, err)
		}
	})
}
func csrf(secret string) string {
	h := sha256.Sum256([]byte("csrf:" + secret))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func (s *Server) sessionCookie() *http.Cookie {
	c := &http.Cookie{Name: "__Host-sandbox-session", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
	origin, err := url.Parse(s.Config.API.PublicOrigin)
	if err == nil && origin.Scheme == "http" && net.ParseIP(origin.Hostname()).IsLoopback() {
		c.Name = gateway.LoopbackSessionCookie
		c.Secure = false
		c.Path = "/v1"
	}
	return c
}
func (s *Server) authenticate(r *http.Request) (auth.Principal, string, error) {
	if v := r.Header.Get("Authorization"); v != "" {
		v, ok := strings.CutPrefix(v, "Bearer ")
		if !ok {
			return auth.Principal{}, "", auth.ErrUnauthenticated
		}
		p, err := s.Auth.Authenticate(r.Context(), v)
		return p, "", err
	}
	c, err := r.Cookie(s.sessionCookie().Name)
	if err != nil {
		return auth.Principal{}, "", auth.ErrUnauthenticated
	}
	p, err := s.Auth.AuthenticateSession(r.Context(), c.Value)
	return p, c.Value, err
}
func write(w http.ResponseWriter, status int, v any) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, e = w.Write(raw)
	return e
}
func fail(w http.ResponseWriter, r *http.Request, err error) {
	code, status := "INTERNAL", 500
	switch {
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, registry.ErrUnauthorized):
		code, status = "UNAUTHENTICATED", 401
	case errors.Is(err, auth.ErrForbidden):
		code, status = "PERMISSION_DENIED", 403
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		code, status = "NOT_FOUND", 404
	case errors.Is(err, registry.ErrInvalid), errors.Is(err, rt.ErrInvalid):
		code, status = "INVALID_ARGUMENT", 400
	case errors.Is(err, registry.ErrControlEpoch):
		code, status = "CONTROL_EPOCH_CHANGED", 409
	case errors.Is(err, registry.ErrIdempotency):
		code, status = "IDEMPOTENCY_CONFLICT", 409
	case errors.Is(err, registry.ErrRevision):
		code, status = "REVISION_CONFLICT", 409
	case errors.Is(err, registry.ErrGeneration):
		code, status = "GENERATION_CONFLICT", 409
	case errors.Is(err, registry.ErrOperationActive), errors.Is(err, registry.ErrOperationState):
		code, status = "OPERATION_IN_PROGRESS", 409
	case errors.Is(err, registry.ErrConflict):
		code, status = "CONFLICT", 409
	case errors.Is(err, registry.ErrMaintenance):
		code, status = "MAINTENANCE", 503
	case errors.Is(err, registry.ErrCursorExpired):
		code, status = "EVENT_CURSOR_EXPIRED", 410
	case errors.Is(err, rt.ErrUnavailable):
		code, status = "RUNTIME_UNAVAILABLE", 503
	case errors.Is(err, rt.ErrUnsupported):
		code, status = "NOT_SUPPORTED", 409
	default:
		var x *lifecycle.Failure
		if errors.As(err, &x) {
			code = x.Code
			status = 409
		}
	}
	msg := err.Error()
	if status == 500 {
		slog.Error("management request failed", "method", r.Method, "error", err)
		msg = "internal error"
	}
	id, _ := registry.NewID("req_")
	detail := map[string]any{"request_id": id, "code": code, "message": msg, "effect": "none", "retry": "after_precondition"}
	if status == 500 {
		detail["effect"] = "unknown"
		detail["retry"] = "client_decision"
	}
	var active *registry.ActiveOperationError
	if errors.As(err, &active) {
		detail["operation_id"] = active.OperationID
		detail["retry"] = "poll_operation"
	}
	_ = write(w, status, map[string]any{"error": detail})
}
func decode(r *http.Request, v any) error {
	b, e := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return registry.ErrInvalid
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return fmt.Errorf("%w: %s", registry.ErrInvalid, e)
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return registry.ErrInvalid
	}
	return nil
}
func revision(r *http.Request) (int64, error) {
	v := r.Header.Get("If-Match")
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		return 0, fmt.Errorf("%w: expected If-Match with quoted revision", registry.ErrInvalid)
	}
	n, e := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	if e != nil || n < 1 {
		return 0, registry.ErrInvalid
	}
	return n, nil
}
func envelope(r *http.Request, p auth.Principal, v any) registry.MutationRequest {
	return registry.MutationRequest{PrincipalID: p.ID, CredentialID: p.CredentialID, Epoch: r.Header.Get("If-Control-Epoch"), Key: r.Header.Get("Idempotency-Key"), Scope: r.Method + " " + r.URL.EscapedPath(), Request: lifecycle.JSON(v)}
}
func (s *Server) accept(w http.ResponseWriter, r *http.Request, p auth.Principal, v any, req registry.AcceptRequest) error {
	env := envelope(r, p, v)
	req.PrincipalID = p.ID
	req.CredentialID = p.CredentialID
	req.Epoch = env.Epoch
	req.Key = env.Key
	req.Scope = env.Scope
	req.Request = env.Request
	if req.MachineID != "" {
		rev, err := revision(r)
		if err != nil {
			return err
		}
		req.ExpectedRevision = rev
	}
	op, _, err := s.DB.Accept(r.Context(), req)
	var busy *registry.ActiveOperationError
	if req.Kind == "task_start" && errors.As(err, &busy) {
		active, readErr := s.DB.GetOperation(r.Context(), busy.OperationID)
		if readErr != nil {
			return readErr
		}
		if active.Kind == "idle_pause" || active.Kind == "sync_keys" {
			_, release, wakeErr := s.Access(r.Context(), p, req.MachineID)
			if wakeErr != nil {
				return wakeErr
			}
			defer release()
			op, _, err = s.DB.Accept(r.Context(), req)
		}
	}
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	return write(w, 202, op)
}
func (s *Server) applyMutation(r *http.Request, p auth.Principal, v any, fn func(context.Context, *sql.Tx) (registry.MutationResult, error)) (json.RawMessage, error) {
	env := envelope(r, p, v)
	if r.Header.Get("If-Match") != "" {
		rev, err := revision(r)
		if err != nil {
			return nil, err
		}
		env.ExpectedRevision = rev
	}
	raw, _, err := s.DB.Mutate(r.Context(), env, fn)
	return raw, err
}
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, p auth.Principal, v any, fn func(context.Context, *sql.Tx) (registry.MutationResult, error)) error {
	raw, err := s.applyMutation(r, p, v, fn)
	if err != nil {
		return err
	}
	return write(w, 200, raw)
}

func list[T any](w http.ResponseWriter, r *http.Request, items []T, id func(T) string) error {
	q := r.URL.Query()
	limit := 100
	var e error
	if q.Has("limit") {
		limit, e = strconv.Atoi(q.Get("limit"))
		if e != nil || limit < 1 || limit > 500 {
			return registry.ErrInvalid
		}
	}
	pos := 0
	if cursor := q.Get("cursor"); cursor != "" {
		for pos < len(items) && id(items[pos]) != cursor {
			pos++
		}
		if pos == len(items) {
			return registry.ErrInvalid
		}
		pos++
	}
	end := min(len(items), pos+limit)
	next := ""
	if end < len(items) {
		next = id(items[end-1])
	}
	if items == nil {
		items = []T{}
	}
	return write(w, 200, map[string]any{"items": items[pos:end], "next_cursor": next})
}
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.Config.API.PublicOrigin {
			fail(w, r, auth.ErrForbidden)
			return
		}
		select {
		case s.loginSlots <- struct{}{}:
			defer func() { <-s.loginSlots }()
		default:
			http.Error(w, "login busy", 429)
			return
		}
		var v struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decode(r, &v); err != nil {
			fail(w, r, err)
			return
		}
		p, err := s.Auth.Login(r.Context(), v.Username, v.Password)
		if err != nil {
			fail(w, r, err)
			return
		}
		secret, err := s.Auth.Session(r.Context(), p)
		if err != nil {
			fail(w, r, err)
			return
		}
		c := s.sessionCookie()
		c.Value, c.MaxAge = secret, 43200
		http.SetCookie(w, c)
		_ = write(w, 200, map[string]any{"principal": p, "csrf_token": csrf(secret)})
	})
	s.route("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		tx, err := s.DB.DB().BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.Auth.RevokeTx(r.Context(), tx, p, p.CredentialID, "web_session", 0); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		c := s.sessionCookie()
		c.MaxAge = -1
		http.SetCookie(w, c)
		return write(w, 200, map[string]bool{"logged_out": true})
	})
	s.route("GET /v1/control", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		c, err := s.DB.Control(r.Context())
		if err != nil {
			return err
		}
		official := s.OfficialRuntime()
		h, healthErr := official.Health(r.Context())
		ready := healthErr == nil && official.Ready(r.Context()) == nil
		v := map[string]any{"control_epoch": c.ControlEpoch, "build": buildinfo.Version, "runtime_version": h.Version, "runtime_ready": ready, "maintenance": c.Maintenance || !ready, "capabilities": rt.FullStateCapabilities(ready), "principal": p}
		if _, secret, _ := s.authenticate(r); secret != "" {
			v["csrf_token"] = csrf(secret)
		}
		return write(w, 200, v)
	})
	s.route("GET /v1/tokens", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		tokens, err := s.Auth.ListTokens(r.Context(), p)
		if err != nil {
			return err
		}
		return list(w, r, tokens, func(t auth.Token) string { return t.ID })
	})
	s.route("POST /v1/tokens", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionTokens, ""); err != nil {
			return err
		}
		var v struct {
			Label     string     `json:"label"`
			Scope     auth.Scope `json:"scope"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if err := decode(r, &v); err != nil {
			return err
		}
		type issueReceipt struct {
			auth.Token
			Secret string `json:"token,omitempty"`
		}
		raw, err := s.applyMutation(r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			t, secret, err := s.Auth.IssueTokenTx(ctx, tx, p, v.Label, v.Scope, v.ExpiresAt)
			return registry.MutationResult{
				Response:      lifecycle.JSON(issueReceipt{Token: t, Secret: secret}),
				RetryResponse: lifecycle.JSON(t),
			}, err
		})
		if err != nil {
			return err
		}
		var receipt issueReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		// Earlier releases saved issuance receipts without a revision.
		if receipt.Revision == 0 {
			receipt.Revision = 1
		}
		return write(w, http.StatusOK, receipt)
	})
	s.route("DELETE /v1/tokens/{token}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionTokens, ""); err != nil {
			return err
		}
		return s.revokeCredential(w, r, p, r.PathValue("token"), "api_token", "token_id")
	})
	s.clientCertRoutes()
	s.settingsRoutes()
	s.previewRoutes()
	s.leaseRoutes()
	s.serviceRoutes()
	s.snapshotRoutes()
	s.artifactRoutes()
	s.taskRoutes()
	s.execRoutes()
	s.fileRoutes()
	s.machineRoutes()
	s.operationsRoutes()
	s.eventRoutes()
}
func (s *Server) machineRoutes() {
	s.route("GET /v1/machines", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		ms, err := s.DB.ListMachines(r.Context())
		if err != nil {
			return err
		}
		visible := make([]machineView, 0, len(ms))
		for _, m := range ms {
			if p.Allows("read", m.ID) {
				v, err := s.viewMachine(r.Context(), m)
				if err != nil {
					return err
				}
				visible = append(visible, v)
			}
		}
		return list(w, r, visible, func(m machineView) string { return m.ID })
	})
	s.route("GET /v1/machines/{machine}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(r.Context(), id)
		if err != nil {
			return err
		}
		v, err := s.viewMachine(r.Context(), m)
		if err != nil {
			return err
		}
		return write(w, 200, v)
	})
	s.route("POST /v1/machines", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("manage", ""); err != nil {
			return err
		}
		var v lifecycle.Create
		if err := decode(r, &v); err != nil {
			return err
		}
		if v.Name == "" || v.TemplateID == "" {
			return registry.ErrInvalid
		}
		if v.Resources.CPUs == 0 {
			v.Resources.CPUs = 1
		}
		if v.Resources.Memory == 0 {
			v.Resources.Memory = 512
		}
		if v.Resources.Storage == 0 {
			v.Resources.Storage = 2
		}
		if v.Resources.Overlay == 0 {
			v.Resources.Overlay = 2
		}
		req := registry.AcceptRequest{Kind: "create", Name: v.Name, TemplateID: v.TemplateID, Heavy: true}
		if v.IdlePolicy == nil {
			tpl, err := s.DB.GetTemplate(r.Context(), v.TemplateID)
			if err != nil {
				return err
			}
			defaults, err := s.settings(r.Context())
			if err != nil {
				return err
			}
			req.DefaultIdlePolicy = lifecycle.JSON(lifecycle.IdlePolicy{Enabled: tpl.GuestRelease != "", Timeout: defaults.IdleSeconds})
		}
		if v.IdlePolicy != nil {
			if v.IdlePolicy.Enabled && v.IdlePolicy.Timeout < 1 {
				return registry.ErrInvalid
			}
			req.IdlePolicy = lifecycle.JSON(v.IdlePolicy)
		}
		return s.accept(w, r, p, v, req)
	})
	for _, kind := range []string{"start", "stop", "resume", "wake", "pause", "delete", "fork"} {
		path := "POST /v1/machines/{machine}/" + kind
		if kind == "delete" {
			path = "DELETE /v1/machines/{machine}"
		}
		if kind == "fork" {
			path = "POST /v1/machines/{machine}/forks"
		}
		s.route(path, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			id := r.PathValue("machine")
			if err := p.Authorize("manage", id); err != nil {
				return err
			}
			req := registry.AcceptRequest{Kind: kind, MachineID: id, Heavy: kind != "delete" && kind != "stop"}
			if kind == "fork" {
				if p.Scope.MachineID != "" {
					return auth.ErrForbidden
				}
				var v struct {
					ChildName          string `json:"child_name"`
					ExpectedGeneration int64  `json:"expected_generation"`
				}
				if err := decode(r, &v); err != nil {
					return err
				}
				req.Name = v.ChildName
				req.ExpectedGeneration = v.ExpectedGeneration
				return s.accept(w, r, p, v, req)
			}
			var v lifecycle.Action
			if err := decode(r, &v); err != nil {
				return err
			}
			req.ExpectedGeneration = v.ExpectedGeneration
			return s.accept(w, r, p, v, req)
		})
	}
	s.route("GET /v1/templates", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("read", ""); err != nil {
			return err
		}
		ts, err := s.DB.ListTemplates(r.Context())
		if err != nil {
			return err
		}
		return list(w, r, ts, func(t registry.Template) string { return t.ID })
	})
	s.route("GET /v1/operations/{op}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		op, err := s.DB.GetOperation(r.Context(), r.PathValue("op"))
		if err != nil {
			return err
		}
		if err = p.Authorize("read", op.MachineID); err != nil {
			return err
		}
		return write(w, 200, op)
	})
	s.metricsRoutes()
}
func (s *Server) eventRoutes() {
	s.route("GET /v1/events", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if p.Scope.Access == auth.AccessTunnel {
			return auth.ErrForbidden
		}
		cursor := r.Header.Get("Last-Event-ID")
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		started := false
		for {
			events, next, err := s.DB.Events(r.Context(), cursor, 200)
			if err != nil {
				if started {
					return nil
				}
				return err
			}
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				started = true
			}
			for _, ev := range events {
				if p.Allows("read", ev.MachineID) {
					if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, lifecycle.JSON(ev)); err != nil {
						return nil
					}
				}
			}
			cursor = next
			fmt.Fprint(w, ": heartbeat\n\n")
			if err = http.NewResponseController(w).Flush(); err != nil {
				return nil
			}
			select {
			case <-r.Context().Done():
				return nil
			case <-tick.C:
			}
			if _, _, err = s.authenticate(r); err != nil {
				return nil
			}
		}
	})
}
func (s *Server) ManagementHost() string { u, _ := url.Parse(s.Config.API.PublicOrigin); return u.Host }
