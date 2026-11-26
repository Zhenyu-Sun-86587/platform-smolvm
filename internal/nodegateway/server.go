package nodegateway

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"sandbox-platform/internal/guesttransport"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type Route struct {
	Reader               string
	LoadManifest         func() (json.RawMessage, error)
	Adapter              rt.Adapter
	Worker               workerclient.Client
	ResolveNetwork       func(*rt.LaunchNetwork) (*rt.LaunchNetwork, error)
	RootGenerations      map[string]bool
	GuestRelayDir        string
	verifyGuestTransport func(context.Context, workerclient.Execution, *net.UnixConn) error
}

type Server struct {
	authority    nodeproto.Authority
	routes       map[nodeproto.Route]Route
	journal      *Journal
	session      string
	bootID       string
	control      chan struct{}
	guest        chan struct{}
	streams      chan struct{}
	storage      chan struct{}
	relayStreams chan struct{}
	relays       guestRelays
	streamMu     sync.Mutex
	perVM        map[string]int
}

func New(authority nodeproto.Authority, routes map[nodeproto.Route]Route, journal *Journal) (*Server, error) {
	if !authority.Valid() || journal == nil || len(routes) == 0 {
		return nil, errors.New("Node requires authority, journal and at least one registered runtime route")
	}
	var journalAuthority string
	expected, err := json.Marshal(authority)
	if err != nil {
		return nil, err
	}
	if err := journal.db.QueryRow("SELECT value FROM authority WHERE id=1").Scan(&journalAuthority); err != nil {
		return nil, err
	}
	if journalAuthority != string(expected) {
		return nil, errors.New("Node authority does not match the journal")
	}
	rawBootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	bootID := strings.TrimSpace(string(rawBootID))
	if !nodeproto.ValidID(bootID) {
		return nil, errors.New("Worker host boot identity is invalid")
	}
	copyRoutes := make(map[nodeproto.Route]Route, len(routes))
	global := make(map[string]bool, len(routes))
	local := make(map[[2]string]bool, len(routes))
	for scope, route := range routes {
		key := [2]string{scope.LocalInstanceID, scope.BundleDigest}
		if !scope.Valid() || route.Adapter == nil || (route.Worker.Socket != "" && route.Worker.Bundle != scope.BundleDigest) ||
			global[scope.RuntimeInstanceID] || local[key] {
			return nil, errors.New("duplicate, incomplete or conflicting runtime route")
		}
		if route.GuestRelayDir != "" {
			if !canonicalAbsolutePath(route.GuestRelayDir) || route.Worker.Socket == "" {
				return nil, errors.New("Guest relay requires a Worker and a canonical directory")
			}
			if err := guesttransport.EnsureDirectory(route.GuestRelayDir); err != nil {
				return nil, err
			}
		}
		global[scope.RuntimeInstanceID], local[key] = true, true
		copyRoutes[scope] = route
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return &Server{
		authority: authority, routes: copyRoutes, journal: journal, session: hex.EncodeToString(id[:]), bootID: bootID,
		control: make(chan struct{}, nodeproto.ControlSlots), guest: make(chan struct{}, nodeproto.GuestSlots),
		storage: make(chan struct{}, nodeproto.StorageSlots), streams: make(chan struct{}, nodeproto.StreamSlots), relayStreams: make(chan struct{}, nodeproto.StreamSlots), perVM: make(map[string]int),
	}, nil
}

func (s *Server) TLSConfig(certificate tls.Certificate, roots *x509.CertPool, controllerPin string) (*tls.Config, error) {
	if roots == nil || certificate.Leaf == nil || !nodeproto.ValidDigest(controllerPin) {
		return nil, errors.New("Node TLS requires certificate, controller trust roots and a public key pin")
	}
	uri := nodeproto.WorkerURI(s.authority)
	if !slices.ContainsFunc(certificate.Leaf.URIs, func(certURI *url.URL) bool { return certURI.String() == uri }) {
		return nil, errors.New("Node TLS certificate is missing its enrolled Worker URI")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
		Certificates: []tls.Certificate{certificate},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
				return errors.New("controller certificate must be verified")
			}
			return nodeproto.VerifyPeer(cs.PeerCertificates[0], controllerPin, nodeproto.ControllerURI(s.authority))
		},
		// The framed streams upgrade on HTTP/1.1.
		NextProtos: []string{"http/1.1"},
	}, nil
}

func (s *Server) Handler(controllerPin string) http.Handler {
	mux := http.NewServeMux()
	rootRPC(s, mux, nodeproto.HandshakePath, func(context.Context, nodeproto.Empty) (nodeproto.Handshake, error) {
		return s.handshake(), nil
	})
	rootRPC(s, mux, nodeproto.GatePath, func(ctx context.Context, _ nodeproto.Empty) (nodeproto.Gate, error) {
		return readGate(ctx, s.journal.db)
	})
	rootRPC(s, mux, nodeproto.SetGatePath, s.journal.setGate)
	s.registerWorkerRPC(mux)
	s.registerBackups(mux)
	s.registerRuntimeRPC(mux)
	s.registerVolumeRPC(mux)
	s.registerStreams(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 ||
			nodeproto.VerifyPeer(r.TLS.PeerCertificates[0], controllerPin, nodeproto.ControllerURI(s.authority)) != nil {
			writeFailure(w, nodeproto.CodeAuthority)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handshake() nodeproto.Handshake {
	routes := make([]nodeproto.RouteInfo, 0, len(s.routes))
	for id, route := range s.routes {
		info := nodeproto.RouteInfo{Route: id, ServiceDial: route.Worker.Socket != "", GuestRelay: route.GuestRelayDir != ""}
		if a, ok := route.Adapter.(interface{ NativeStopContract() bool }); ok {
			info.NativeStop = a.NativeStopContract()
		}
		if a, ok := route.Adapter.(interface{ NativeStandbyContract() bool }); ok {
			info.NativeStandby = a.NativeStandbyContract()
		}
		if a, ok := route.Adapter.(interface{ Admit(string) error }); ok {
			for _, action := range []string{"configure", "delete", "service_create", "service_delete", "service_update", "task_cancel", "task_start", rt.VMSnapshotDelete} {
				if a.Admit(action) == nil {
					info.AdmittedActions = append(info.AdmittedActions, action)
				}
			}
		}
		routes = append(routes, info)
	}
	slices.SortFunc(routes, func(a, b nodeproto.RouteInfo) int {
		return strings.Compare(a.RuntimeInstanceID, b.RuntimeInstanceID)
	})
	return nodeproto.Handshake{Protocol: nodeproto.Version, Authority: s.authority, NodeSession: s.session,
		HostBootID: s.bootID, Routes: routes, Coverage: nodeproto.KnownOnly}
}

func writeFailure(w http.ResponseWriter, code string) {
	writeRPCFailure(w, &nodeproto.Failure{Code: code})
}

func writeRPCFailure(w http.ResponseWriter, failure *nodeproto.Failure) {
	status := http.StatusServiceUnavailable
	switch failure.Code {
	case nodeproto.CodeInvalid:
		status = http.StatusBadRequest
	case nodeproto.CodeAuthority, nodeproto.CodeForbidden:
		status = http.StatusForbidden
	case nodeproto.CodeRoute, nodeproto.CodeNotFound:
		status = http.StatusNotFound
	case nodeproto.CodeConflict, nodeproto.CodeGateClosed, nodeproto.CodeNativeRefused:
		status = http.StatusConflict
	case nodeproto.CodeUnsupported:
		status = http.StatusNotImplemented
	case nodeproto.CodeBusy:
		status = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(failure)
}

func decodeJSON(in io.Reader, target any) error {
	d := json.NewDecoder(in)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

func (s *Server) checkScope(protocol string, authority nodeproto.Authority, scope nodeproto.Route, routeRequired bool) (Route, error) {
	if protocol != nodeproto.Version {
		return Route{}, &nodeproto.Failure{Code: nodeproto.CodeUnsupported}
	}
	if authority != s.authority {
		return Route{}, &nodeproto.Failure{Code: nodeproto.CodeAuthority}
	}
	if !routeRequired && scope == (nodeproto.Route{}) {
		return Route{}, nil
	}
	if route, ok := s.routes[scope]; ok && routeRequired {
		return route, nil
	}
	return Route{}, &nodeproto.Failure{Code: nodeproto.CodeRoute}
}

func takeSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func serverRPC[In, Out any](s *Server, mux *http.ServeMux, path string, slots chan struct{}, timeout time.Duration, routeRequired bool, f func(context.Context, nodeproto.Route, Route, In) (Out, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			writeFailure(w, nodeproto.CodeInvalid)
			return
		}
		if !takeSlot(slots) {
			writeFailure(w, nodeproto.CodeBusy)
			return
		}
		defer func() { <-slots }()
		var in nodeproto.Envelope[In]
		body := http.MaxBytesReader(w, r.Body, nodeproto.ControlRequestLimit)
		if err := decodeJSON(body, &in); err != nil {
			writeFailure(w, nodeproto.CodeInvalid)
			return
		}
		route, err := s.checkScope(in.Protocol, in.Authority, in.Route, routeRequired)
		if err != nil {
			writeFailure(w, nodeproto.FailureCode(err))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		out, err := f(ctx, in.Route, route, in.Request)
		if err != nil {
			var failure *nodeproto.Failure
			if errors.As(err, &failure) {
				writeRPCFailure(w, failure)
			} else {
				writeFailure(w, nodeproto.FailureCode(err))
			}
			return
		}
		raw, err := json.Marshal(out)
		if err != nil || len(raw) > nodeproto.ControlReplyLimit {
			writeFailure(w, nodeproto.CodeUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, _ = w.Write(raw)
	})
}

func rootRPC[In, Out any](s *Server, mux *http.ServeMux, path string, f func(context.Context, In) (Out, error)) {
	serverRPC(s, mux, path, s.control, 15*time.Second, false, func(ctx context.Context, _ nodeproto.Route, _ Route, in In) (Out, error) {
		return f(ctx, in)
	})
}

func rpc[In, Out any](s *Server, mux *http.ServeMux, path string, f func(context.Context, Route, In) (Out, error)) {
	serverRPC(s, mux, path, s.control, 15*time.Second, true, func(ctx context.Context, _ nodeproto.Route, route Route, in In) (Out, error) {
		return f(ctx, route, in)
	})
}
