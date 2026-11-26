package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/servicehealth"
)

type service struct {
	Observation *servicehealth.Observation `json:"observation,omitempty"`
	Readiness   readiness                  `json:"readiness"`
	Auth        accessPolicy               `json:"auth"`
	SSHKey      string                     `json:"ssh_host_key,omitempty"`
	ID          string                     `json:"service_id"`
	MachineID   string                     `json:"machine_id"`
	Name        string                     `json:"name"`
	Revision    int64                      `json:"revision"`
	Protocol    string                     `json:"protocol"`
	GuestPort   uint16                     `json:"guest_port"`
	PublicHost  string                     `json:"public_host,omitempty"`
	PublicPort  int                        `json:"public_port,omitempty"`
	Enabled     bool                       `json:"enabled"`
}

const serviceCols = "id,machine_id,name,revision,kind,guest_port,coalesce(public_host,''),coalesce(public_port,0),enabled,readiness_json,access_json"

type row interface{ Scan(...any) error }

func readService(r row) (service, error) {
	var v service
	var raw, access string
	err := r.Scan(&v.ID, &v.MachineID, &v.Name, &v.Revision, &v.Protocol, &v.GuestPort, &v.PublicHost, &v.PublicPort, &v.Enabled, &raw, &access)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &v.Readiness)
	}
	if err == nil {
		err = json.Unmarshal([]byte(access), &v.Auth)
	}
	return v, err
}
func (s *Server) serviceIdentity(ctx context.Context, v *service) error {
	if v.Protocol != "ssh" {
		return nil
	}
	m, err := s.DB.GetMachine(ctx, v.MachineID)
	if err != nil {
		return err
	}
	if m.CurrentBinding == nil || m.DeletedAt != nil {
		return nil
	}
	if m.CurrentBinding.RuntimeBundleID != "" {
		v.SSHKey, err = s.VerifiedNativeSSHKey(ctx, *m.CurrentBinding, v.GuestPort)
		return err
	}
	var e struct {
		Ready bool   `json:"ssh_ready"`
		Key   string `json:"ssh_host_key"`
	}
	if err = json.Unmarshal(m.CurrentBinding.GuestManagementEvidence, &e); err != nil {
		return err
	}
	if e.Ready {
		v.SSHKey = e.Key
	}
	return nil
}
func (s *Server) getService(ctx context.Context, id string) (service, error) {
	v, err := readService(s.DB.ReadDB().QueryRowContext(ctx, "SELECT "+serviceCols+" FROM services WHERE id=?", id))
	if err == nil {
		err = s.serviceIdentity(ctx, &v)
	}
	return v, err
}

type serviceEdit struct {
	Readiness          *readiness    `json:"readiness,omitempty"`
	Auth               *accessPolicy `json:"auth,omitempty"`
	ID                 string        `json:"-"`
	Name               string        `json:"name"`
	GuestPort          uint16        `json:"guest_port"`
	Protocol           string        `json:"protocol"`
	ExpectedGeneration int64         `json:"expected_generation,omitempty"`
}
type ticket struct {
	Origin, Target, Secret string
	Browser                bool
	Expiry                 time.Time
}

func (s *Server) serviceRoutes() {
	s.servicePatchRoute()
	s.Extra["service_create"] = s.editService
	s.Extra["service_delete"] = s.editService
	for _, kind := range []string{"service_create", "service_update", "service_delete"} {
		s.Recover[kind] = func(ctx context.Context, j *lifecycle.Job) error {
			if privateServiceWorker(j.Runtime) == "" {
				return errors.New("dispatched operation needs specific outcome evidence")
			}
			j.Written = false
			return s.Extra[j.Op.Kind](ctx, j)
		}
	}
	s.route("POST /v1/machines/{machine}/services", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		mid := r.PathValue("machine")
		if err := p.Authorize("manage", mid); err != nil {
			return err
		}
		var v serviceEdit
		if err := decode(r, &v); err != nil {
			return err
		}
		if err := v.validate(); err != nil {
			return err
		}
		if len(v.Name) == 0 || len(v.Name) > 100 || v.GuestPort == 0 {
			return registry.ErrInvalid
		}
		switch v.Protocol {
		case "http", "https", "ssh", "tcp-mtls", "vnc":
		default:
			return registry.ErrInvalid
		}
		if v.Protocol == "tcp-mtls" && (s.Config.Gateway.ClientCA == "" || s.Config.API.TLSCert == "") {
			return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "TCP listener requires configured mTLS client CA"}
		}
		return s.accept(w, r, p, v, registry.AcceptRequest{Kind: "service_create", MachineID: mid, ExpectedGeneration: v.ExpectedGeneration})
	})
	s.route("DELETE /v1/services/{svc}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		rev, err := revision(r)
		if err != nil {
			return err
		}
		var body struct{}
		if err = decode(r, &body); err != nil {
			return err
		}
		id := r.PathValue("svc")
		return s.acceptServiceChange(w, r, p, id, rev, "service_delete", map[string]any{"service_id": id, "revision": rev})
	})
	s.route("GET /v1/machines/{machine}/services", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		mid := r.PathValue("machine")
		if err := p.Authorize("read", mid); err != nil {
			return err
		}
		rows, err := s.DB.ReadDB().QueryContext(r.Context(), "SELECT "+serviceCols+" FROM services WHERE machine_id=? ORDER BY id", mid)
		if err != nil {
			return err
		}
		defer rows.Close()
		out := []service{}
		for rows.Next() {
			v, e := readService(rows)
			if e != nil {
				return e
			}
			if e = s.serviceIdentity(r.Context(), &v); e != nil {
				return e
			}
			out = append(out, v)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(out) > 0 {
			m, err := s.DB.GetMachine(r.Context(), mid)
			if err != nil {
				return err
			}
			for i := range out {
				s.projectServiceObservation(&out[i], m)
			}
		}
		return list(w, r, out, func(v service) string { return v.ID })
	})
	s.route("GET /v1/services/{svc}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		v, err := s.getService(r.Context(), r.PathValue("svc"))
		if err != nil {
			return err
		}
		if err = p.Authorize("read", v.MachineID); err != nil {
			return err
		}
		m, err := s.DB.GetMachine(r.Context(), v.MachineID)
		if err != nil {
			return err
		}
		s.projectServiceObservation(&v, m)
		return write(w, 200, v)
	})
	s.route("POST /v1/services/{svc}/tunnel-tickets", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		v, err := s.getService(r.Context(), r.PathValue("svc"))
		if err != nil {
			return err
		}
		if err = p.Authorize("tunnel", v.MachineID); err != nil {
			return err
		}
		desktop, err := s.requiresDesktopSession(r.Context(), v)
		if err != nil {
			return err
		}
		if desktop {
			return &lifecycle.Failure{Code: "DESKTOP_SESSION_REQUIRED", Message: "VNC tickets require a desktop session"}
		}
		return s.issueTicket(w, r, v.ID)
	})
	s.mux.HandleFunc("GET /v1/services/{svc}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.getService(r.Context(), r.PathValue("svc"))
		if err == nil {
			err = s.tunnel(w, r, v)
		}
		if err != nil {
			fail(w, r, err)
		}
	})
	s.mux.HandleFunc("GET /v1/machines/{machine}/ssh-tunnel", func(w http.ResponseWriter, r *http.Request) {
		mid := r.PathValue("machine")
		p, _, err := s.authenticate(r)
		if err == nil {
			err = p.Authorize("tunnel", mid)
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		v, err := readService(s.DB.ReadDB().QueryRowContext(r.Context(), "SELECT "+serviceCols+" FROM services WHERE machine_id=? AND kind='ssh' AND enabled=1 ORDER BY id LIMIT 1", mid))
		if err == nil {
			err = s.tunnel(w, r, v)
		}
		if err != nil {
			fail(w, r, err)
		}
	})
}
func (s *Server) issueTicket(w http.ResponseWriter, r *http.Request, target string) error {
	if r.Header.Get("Origin") != s.Config.API.PublicOrigin {
		return auth.ErrForbidden
	}
	_, secret, err := s.authenticate(r)
	if err != nil {
		return err
	}
	browser := secret != ""
	if !browser {
		secret = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	key, err := registry.NewID("ws_")
	if err != nil {
		return err
	}
	expiry := time.Now().Add(30 * time.Second)
	s.tickets.Store(key, ticket{s.Config.API.PublicOrigin, target, secret, browser, expiry})
	time.AfterFunc(30*time.Second, func() { s.tickets.Delete(key) })
	return write(w, 200, map[string]any{"ticket": key, "expires_at": expiry.UTC()})
}
func (s *Server) streamPrincipal(r *http.Request, target string) (auth.Principal, error) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.Config.API.PublicOrigin {
		return auth.Principal{}, auth.ErrForbidden
	}
	if r.Header.Get("Authorization") != "" {
		p, _, err := s.authenticate(r)
		return p, err
	}
	for _, proto := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		proto = strings.TrimSpace(proto)
		key, ok := strings.CutPrefix(proto, "ticket.")
		if !ok {
			continue
		}
		v, ok := s.tickets.LoadAndDelete(key)
		if !ok {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
		t, ok := v.(ticket)
		if !ok {
			return auth.Principal{}, auth.ErrForbidden
		}
		if t.Target != target || t.Origin != r.Header.Get("Origin") || time.Now().After(t.Expiry) {
			return auth.Principal{}, auth.ErrForbidden
		}
		if t.Browser {
			return s.Auth.AuthenticateSession(r.Context(), t.Secret)
		}
		return s.Auth.Authenticate(r.Context(), t.Secret)
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}
func (s *Server) tunnel(w http.ResponseWriter, r *http.Request, v service) error {
	p, err := s.streamPrincipal(r, v.ID)
	if err != nil {
		return err
	}
	desktop, err := s.requiresDesktopSession(r.Context(), v)
	if err != nil {
		return err
	}
	if desktop {
		return &lifecycle.Failure{Code: "DESKTOP_SESSION_REQUIRED", Message: "VNC tunnels require a desktop session"}
	}
	return s.tunnelAuthorized(w, r, v, p, nil)
}

func (s *Server) tunnelAuthorized(w http.ResponseWriter, r *http.Request, v service, p auth.Principal, guard func(context.Context) error) error {
	if err := p.Authorize("tunnel", v.MachineID); err != nil {
		return err
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return registry.ErrInvalid
	}
	if guard != nil {
		if err := guard(r.Context()); err != nil {
			return err
		}
	}
	backend, check, release, err := s.resolve(r.Context(), p, v)
	if err != nil {
		return err
	}
	defer release()
	if guard != nil {
		checkBackend := check
		check = func(ctx context.Context) error {
			if err := guard(ctx); err != nil {
				return err
			}
			return checkBackend(ctx)
		}
	}
	protocol, tunnel := "sandbox.tunnel.v1", backend.Tunnel
	if guard != nil {
		protocol, tunnel = "sandbox.rfb.v1", backend.TunnelRFB
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
	if err != nil {
		return nil
	}
	defer conn.CloseNow()
	if err = tunnel(r.Context(), conn, check); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "backend closed")
	}
	return nil
}
func (s *Server) resolve(ctx context.Context, p auth.Principal, v service) (gateway.Backend, func(context.Context) error, func(), error) {
	var empty gateway.Backend
	if !v.Enabled {
		return empty, nil, nil, registry.ErrConflict
	}
	reauthorize := func(ctx context.Context) error {
		current, err := s.getService(ctx, v.ID)
		if err != nil {
			return err
		}
		if !current.Enabled || current.Revision != v.Revision {
			return registry.ErrRevision
		}
		return nil
	}
	admission := ctx
	if v.Protocol == "ssh" {
		var cancel context.CancelFunc
		admission, cancel = context.WithTimeout(ctx, time.Duration(s.Config.Gateway.WaitSeconds)*time.Second)
		defer cancel()
	}
	initial, err := s.DB.GetMachine(admission, v.MachineID)
	if err != nil {
		return empty, nil, nil, err
	}
	before := serviceObservationIdentity(v, initial.CurrentBinding)
	stage := servicehealth.StageGuestManagement
	switch servicePower(initial.CurrentBinding) {
	case registry.PowerStandby, registry.PowerHibernated, registry.PowerStopped:
		stage = servicehealth.StageWake
	}
	b, end, err := s.AccessAt(admission, p, v.MachineID, lifecycle.AccessEndpoint{
		Trigger: "service_access", Revision: v.Revision, ServiceID: v.ID, Reauthorize: reauthorize,
	})
	if err != nil {
		obs := servicehealth.Diagnose(stage, err)
		s.serviceHealth.Record(v.ID, before, obs)
		return empty, nil, nil, servicehealth.Annotate(err, obs)
	}
	key := serviceObservationIdentity(v, &b)
	observe := func(obs servicehealth.Observation) {
		s.serviceHealth.Record(v.ID, key, obs)
	}
	fail := func(err error, obs servicehealth.Observation) (gateway.Backend, func(context.Context) error, func(), error) {
		observe(obs)
		end()
		return empty, nil, nil, servicehealth.Annotate(err, obs)
	}
	if stage == servicehealth.StageWake {
		observe(servicehealth.Diagnose(stage, nil))
	}
	adapter, err := s.ForBinding(b)
	if err != nil {
		return fail(err, servicehealth.Diagnose(servicehealth.StageGuestManagement, err))
	}
	if v.Protocol == "ssh" {
		if v.SSHKey, err = s.awaitSSHKey(admission, p, b, v, reauthorize); err != nil {
			return fail(err, servicehealth.Diagnose(servicehealth.StageGuestManagement, err))
		}
		observe(servicehealth.Diagnose(servicehealth.StageGuestManagement, nil))
	}
	checkBinding := func(ctx context.Context) error {
		if p.CredentialID != "" {
			var active bool
			err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM credentials c
				JOIN principals p ON p.id=c.principal_id WHERE c.id=? AND p.id=? AND p.disabled=0
				AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>?))`,
				p.CredentialID, p.ID, time.Now().UnixMilli()).Scan(&active)
			if err != nil {
				return err
			}
			if !active {
				return auth.ErrUnauthenticated
			}
		}
		m, err := s.DB.GetMachine(ctx, v.MachineID)
		if err != nil {
			return err
		}
		if m.DeletedAt != nil || m.CurrentBinding == nil || m.CurrentBinding.ID != b.ID || m.CurrentBinding.Generation != b.Generation ||
			m.CurrentBinding.ExecutionRef() != b.ExecutionRef() {
			return registry.ErrGeneration
		}
		now, err := s.getService(ctx, v.ID)
		if err != nil {
			return err
		}
		if !now.Enabled || v.Revision != now.Revision {
			return registry.ErrRevision
		}
		if b.RuntimeBundleID != "" && v.Protocol == "ssh" && now.SSHKey != v.SSHKey {
			return registry.ErrGeneration
		}
		return nil
	}
	var backend gateway.Backend
	check := checkBinding
	if worker := privateServiceWorker(adapter); worker != "" {
		backend, err = workerServiceBackend(ctx, worker, b, v)
		if err != nil {
			return fail(err, servicehealth.Unavailable(servicehealth.StageWorkerUpstream, "ATTACHMENT_UNAVAILABLE"))
		}
	} else {
		o, err := adapter.Observe(ctx, b.RuntimeName)
		if err != nil {
			return fail(err, servicehealth.Diagnose(servicehealth.StageGuestManagement, err))
		}
		var port uint16
		err = s.DB.ReadDB().QueryRowContext(ctx, "SELECT host_port FROM port_allocations WHERE service_id=? AND binding_id=? AND state='bound'", v.ID, b.ID).Scan(&port)
		if err != nil {
			return fail(err, servicehealth.Unavailable(servicehealth.StageConfiguration, "PORT_PUBLICATION_UNAVAILABLE"))
		}
		backend = gateway.Backend{Port: port, Protocol: v.Protocol, ServerName: v.PublicHost}
		check = func(ctx context.Context) error {
			if err := checkBinding(ctx); err != nil {
				return err
			}
			fresh, err := adapter.Observe(ctx, b.RuntimeName)
			if err != nil {
				return err
			}
			if fresh.Labels["platform.binding"] != b.ID || fresh.State != "running" || !fresh.Identity.Equal(o.Identity) {
				return registry.ErrGeneration
			}
			for _, a := range fresh.Ports {
				if a.Guest == v.GuestPort && a.Host == port && a.Protocol == "tcp" {
					return nil
				}
			}
			return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "runtime port publication changed"}
		}
	}
	if v.Readiness.Type == "http" {
		if err := check(ctx); err != nil {
			return fail(err, servicehealth.Diagnose(servicehealth.StageConfiguration, err))
		}
		err := probeHTTP(ctx, backend, v.Readiness)
		probe := servicehealth.Probe(err)
		if err != nil {
			return fail(err, probe)
		}
		observe(probe)
	}
	backend.Observe = func(err error) string {
		obs := servicehealth.Diagnose(servicehealth.StageWorkerUpstream, err)
		observe(obs)
		return obs.ReasonCode
	}
	return backend, check, end, nil
}

// Keep the Wake hold while the System observer verifies the new execution's
// host key. The gateway has not opened a Guest stream during this wait.
func (s *Server) awaitSSHKey(ctx context.Context, p auth.Principal, b registry.Binding, v service, reauthorize func(context.Context) error) (string, error) {
	notReady := &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "binding SSH identity not yet verified"}
	if b.RuntimeBundleID == "" {
		if err := s.serviceIdentity(ctx, &v); err == nil && v.SSHKey != "" {
			return v.SSHKey, nil
		}
		return "", notReady
	}
	if v.GuestPort != lifecycle.SystemSSHPort {
		return "", notReady
	}
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := s.CheckAccessPrincipal(ctx, p, b.MachineID); err != nil {
			return "", err
		}
		if err := reauthorize(ctx); err != nil {
			return "", err
		}
		m, err := s.DB.GetMachine(ctx, b.MachineID)
		if err != nil {
			return "", err
		}
		if m.DeletedAt != nil || m.CurrentBinding == nil || m.CurrentBinding.ID != b.ID ||
			m.CurrentBinding.ExecutionRef() != b.ExecutionRef() {
			return "", registry.ErrGeneration
		}
		key, err := s.VerifiedNativeSSHKey(ctx, *m.CurrentBinding, v.GuestPort)
		if err != nil || key != "" {
			return key, err
		}
		select {
		case <-ctx.Done():
			return "", notReady
		case <-poll.C:
		}
	}
}

func (s *Server) serviceHTTP(w http.ResponseWriter, r *http.Request, host string) {
	v, err := readService(s.DB.ReadDB().QueryRowContext(r.Context(), "SELECT "+serviceCols+" FROM services WHERE public_host=? AND kind IN ('http','https') AND enabled=1", host))
	if err != nil {
		fail(w, r, auth.ErrUnauthenticated)
		return
	}
	p, handled, err := s.previewAuth(w, r, v)
	if handled {
		return
	}
	if err == nil {
		err = p.Authorize("tunnel", v.MachineID)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	b, check, end, err := s.resolve(r.Context(), p, v)
	if err != nil {
		w.Header().Set(gateway.ForwardingHeader, "not_started")
		fail(w, r, err)
		return
	}
	defer end()
	b.HTTP(w, r, check)
}

func (s *Server) requestedService(j *lifecycle.Job) (service, error) {
	var v serviceEdit
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return service{}, err
	}
	svc := service{ID: "svc-" + j.Op.ID[3:], MachineID: j.Op.MachineID, Name: v.Name, Revision: 1, Protocol: v.Protocol, GuestPort: v.GuestPort}
	if v.Protocol == "http" || v.Protocol == "https" {
		if s.Config.Gateway.AppsDomain == "" {
			return service{}, errors.New("gateway apps_domain is required")
		}
		svc.PublicHost = svc.ID + "." + s.Config.Gateway.AppsDomain
	}
	if v.Readiness != nil {
		svc.Readiness = *v.Readiness
	}
	return svc, nil
}

func (s *Server) insertService(ctx context.Context, tx *sql.Tx, svc service, enabled bool) error {
	var domain any
	if svc.PublicHost != "" {
		domain = svc.PublicHost
	}
	var publicPort any
	if svc.Protocol == "tcp-mtls" {
		rng := s.Config.Gateway.TCPRange
		for n := rng[0]; n <= rng[1]; n++ {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM services WHERE public_port=?", n).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				host, _, _ := net.SplitHostPort(s.Config.API.Listen)
				l, e := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(n)))
				if e == nil {
					l.Close()
					publicPort = n
					break
				}
			}
		}
		if publicPort == nil {
			return errors.New("no free mTLS port")
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO services(id,machine_id,name,revision,kind,guest_port,public_host,public_port,readiness_json,access_json,enabled) VALUES(?,?,?,1,?,?,?,?,?, '{"private":true}',?)`, svc.ID, svc.MachineID, svc.Name, svc.Protocol, svc.GuestPort, domain, publicPort, string(lifecycle.JSON(svc.Readiness)), enabled)
	return err
}

func (s *Server) editService(ctx context.Context, j *lifecycle.Job) error {
	if worker := privateServiceWorker(j.Runtime); worker != "" {
		return s.editWorkerService(ctx, j, worker)
	}
	o, err := j.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if o.Labels["platform.binding"] != j.Binding.ID {
		return registry.ErrGeneration
	}
	var svc service
	var host uint16
	desired := append([]rt.Port{}, o.Ports...)
	if j.Op.Kind == "service_create" {
		svc, err = s.requestedService(j)
		if err != nil {
			return err
		}
		for _, port := range desired {
			if port.Guest == svc.GuestPort {
				return errors.New("guest port already published")
			}
		}
		tx, err := s.DB.DB().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.insertService(ctx, tx, svc, false); err != nil {
			return err
		}
		rng := s.Config.Gateway.BackendRange
		for n := rng[0]; n <= rng[1]; n++ {
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations WHERE protocol='tcp' AND host_port=? AND state!='released'", n).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				continue
			}
			l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(n)))
			if err != nil {
				continue
			}
			l.Close()
			host = uint16(n)
			break
		}
		if host == 0 {
			return errors.New("backend port range exhausted")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO port_allocations(id,binding_id,service_id,protocol,host_port,guest_port,state) VALUES(?,?,?,'tcp',?,?,'allocated')", "port_"+j.Op.ID, j.Binding.ID, svc.ID, host, svc.GuestPort)
		if err != nil {
			return err
		}
		desired = append(desired, rt.Port{Guest: svc.GuestPort, Host: host, Protocol: "tcp"})
		if err = tx.Commit(); err != nil {
			return err
		}
		j.Written = true
	} else {
		var v struct {
			ID       string `json:"service_id"`
			Revision int64  `json:"revision"`
		}
		if err = json.Unmarshal(j.Op.Request, &v); err != nil {
			return err
		}
		svc, err = s.getService(ctx, v.ID)
		if err != nil {
			return err
		}
		if svc.Revision != v.Revision || svc.MachineID != j.Op.MachineID {
			return registry.ErrRevision
		}
		err = s.DB.ReadDB().QueryRowContext(ctx, "SELECT host_port FROM port_allocations WHERE service_id=? AND binding_id=? AND state='bound'", svc.ID, j.Binding.ID).Scan(&host)
		if err != nil {
			return err
		}
		desired = nil
		for _, a := range o.Ports {
			if a.Host != host || a.Guest != svc.GuestPort {
				desired = append(desired, a)
			}
		}
	}
	if err = j.Stage(ctx, "set_ports", ""); err != nil {
		return err
	}
	if _, err = j.Runtime.SetPorts(ctx, j.Binding.RuntimeName, desired); err != nil {
		return err
	}
	j.Written = true
	o, err = j.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	found := false
	for _, a := range o.Ports {
		if a.Host == host && a.Guest == svc.GuestPort {
			found = true
		}
	}
	if found != (j.Op.Kind == "service_create") {
		return fmt.Errorf("port update did not match desired publication")
	}
	obs := lifecycle.Evidence(o)
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Observation: &obs, Result: lifecycle.JSON(map[string]string{"service_id": svc.ID}), Apply: func(ctx context.Context, tx *sql.Tx) error {
		if j.Op.Kind == "service_create" {
			if _, err := tx.ExecContext(ctx, "UPDATE services SET enabled=1 WHERE id=?", svc.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE port_allocations SET state='bound' WHERE service_id=? AND binding_id=?", svc.ID, j.Binding.ID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, "UPDATE port_allocations SET state='released',service_id=NULL WHERE service_id=?", svc.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM services WHERE id=?", svc.ID); err != nil {
				return err
			}
		}
		return registry.AppendEvent(ctx, tx, "service", j.Op.MachineID, j.Op.ID, map[string]string{"service_id": svc.ID}, false)
	}})
}
