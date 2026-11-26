package platformapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/nodes"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) workerEnrollmentRoutes() {
	s.route("POST /v1/workers/{worker}/enrollment/ticket", s.issueWorkerEnrollmentTicket)
	slots := make(chan struct{}, 2)
	s.mux.HandleFunc("POST /v1/workers/{worker}/enrollment/redeem", func(w http.ResponseWriter, r *http.Request) {
		if err := s.redeemWorkerEnrollmentTicket(w, r, slots); err != nil {
			fail(w, r, err)
		}
	})
}

func (s *Server) enrollmentScope(r *http.Request) (config.Node, registry.WorkerEnrollmentScope, error) {
	worker := r.PathValue("worker")
	index := slices.IndexFunc(s.Config.Nodes, func(n config.Node) bool { return n.ID == worker })
	if index < 0 || s.Config.Nodes[index].CASigningKey == "" {
		return config.Node{}, registry.WorkerEnrollmentScope{}, rt.ErrUnsupported
	}
	n := s.Config.Nodes[index]
	control, err := s.DB.Control(r.Context())
	if err != nil {
		return config.Node{}, registry.WorkerEnrollmentScope{}, err
	}
	return n, registry.WorkerEnrollmentScope{WorkerID: n.ID, Generation: int64(n.Generation), ControllerID: control.InstallationID,
		ControlEpoch: control.ControlEpoch, Endpoint: n.Endpoint, SPKIPin: n.Pin}, nil
}

func (s *Server) issueWorkerEnrollmentTicket(w http.ResponseWriter, r *http.Request, principal auth.Principal) error {
	if err := principal.Authorize(auth.ActionAdmin, ""); err != nil {
		return err
	}
	if r.TLS == nil {
		return auth.ErrForbidden
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if err := enrollmentJSON(w, r, &request, 1024); err != nil {
		return err
	}
	if request.Mode != "initial" && request.Mode != "rotate" {
		return registry.ErrInvalid
	}
	n, scope, err := s.enrollmentScope(r)
	if err != nil {
		return err
	}
	if r.Header.Get("If-Control-Epoch") != scope.ControlEpoch {
		return registry.ErrControlEpoch
	}
	if _, err := nodes.LoadEnrollmentSigner(s.Config, n); err != nil {
		return err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	defer clear(secret[:])
	expires, err := s.DB.IssueWorkerEnrollmentTicket(r.Context(), scope, sha256.Sum256(secret[:]), request.Mode)
	if err != nil {
		return err
	}
	return write(w, http.StatusCreated, struct {
		nodeproto.Authority
		Ticket    string    `json:"ticket"`
		ExpiresAt time.Time `json:"expires_at"`
	}{asNodeAuthority(scope), base64.RawURLEncoding.EncodeToString(secret[:]), expires.UTC()})
}

func (s *Server) redeemWorkerEnrollmentTicket(w http.ResponseWriter, r *http.Request, slots chan struct{}) error {
	encoded, ok := strings.CutPrefix(r.Header.Get("Authorization"), "WorkerEnrollment ")
	if r.TLS == nil || !ok || len(encoded) != base64.RawURLEncoding.EncodedLen(32) {
		return auth.ErrUnauthenticated
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return auth.ErrUnauthenticated
	}
	hash := sha256.Sum256(token)
	clear(token)
	n, scope, err := s.enrollmentScope(r)
	if errors.Is(err, rt.ErrUnsupported) {
		return auth.ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	ctx := r.Context()
	if err := s.DB.CheckWorkerEnrollmentTicket(ctx, scope, hash); err != nil {
		return enrollmentTicketFailure(err)
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return auth.ErrGuestQuota
	}
	var request struct {
		CSR string `json:"csr_pem"`
	}
	if err := enrollmentJSON(w, r, &request, 32<<10); err != nil {
		return err
	}
	if len(request.CSR) > 16<<10 {
		return registry.ErrInvalid
	}
	csr, digest, err := nodes.PrepareWorkerCSR(request.CSR, scope.SPKIPin)
	if err != nil {
		return errors.Join(registry.ErrInvalid, err)
	}
	certificate, err := s.DB.WorkerEnrollmentTicketCertificate(ctx, scope, hash, digest)
	if err != nil {
		return enrollmentTicketFailure(err)
	}
	authority := asNodeAuthority(scope)
	if len(certificate) == 0 {
		signer, err := nodes.LoadEnrollmentSigner(s.Config, n)
		if err != nil {
			return err
		}
		certificate, err = s.DB.ConsumeWorkerEnrollmentTicket(ctx, scope, hash, digest, func() ([]byte, error) {
			return signer.Sign(csr, authority)
		})
		if err != nil {
			return enrollmentTicketFailure(err)
		}
	}
	return write(w, http.StatusOK, struct {
		nodeproto.Authority
		Certificate string `json:"certificate_pem"`
	}{authority, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}))})
}

func enrollmentJSON(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return registry.ErrInvalid
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(5 * time.Second)); err == nil {
		defer controller.SetReadDeadline(time.Time{})
	}
	reader := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	reader.DisallowUnknownFields()
	var extra json.RawMessage
	if err := reader.Decode(out); err != nil {
		return registry.ErrInvalid
	}
	if err := reader.Decode(&extra); err != io.EOF {
		return registry.ErrInvalid
	}
	return nil
}

func enrollmentTicketFailure(err error) error {
	if errors.Is(err, registry.ErrWorkerEnrollmentTicket) {
		return auth.ErrUnauthenticated
	}
	return err
}

func asNodeAuthority(scope registry.WorkerEnrollmentScope) nodeproto.Authority {
	return nodeproto.Authority{WorkerID: scope.WorkerID, EnrollmentGeneration: uint64(scope.Generation),
		ControllerID: scope.ControllerID, ControlEpoch: scope.ControlEpoch}
}
