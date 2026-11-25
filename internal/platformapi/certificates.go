package platformapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

type certScope struct {
	ServiceIDs []string `json:"service_ids"`
}
type certRequest struct {
	CSR   string    `json:"csr"`
	Scope certScope `json:"scope"`
	TTL   int       `json:"ttl_seconds"`
}

func (s *Server) clientCertRoutes() {
	s.route("POST /v1/client-certificates", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		var v certRequest
		if err := decode(r, &v); err != nil {
			return err
		}
		if len(v.Scope.ServiceIDs) == 0 || len(v.Scope.ServiceIDs) > 64 || v.TTL < 1 || v.TTL > 365*86400 {
			return registry.ErrInvalid
		}
		block, rest := pem.Decode([]byte(v.CSR))
		if block == nil || len(rest) != 0 || block.Type != "CERTIFICATE REQUEST" {
			return registry.ErrInvalid
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			return registry.ErrInvalid
		}
		pair, err := tls.LoadX509KeyPair(s.Config.Gateway.ClientCA, s.Config.Gateway.ClientCAKey)
		if err != nil {
			return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "client certificate issuer keypair unavailable"}
		}
		issuer, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || !issuer.IsCA || issuer.KeyUsage&x509.KeyUsageCertSign == 0 {
			return errors.New("invalid configured client CA")
		}
		signer, ok := pair.PrivateKey.(crypto.Signer)
		if !ok {
			return errors.New("invalid issuer signer")
		}
		return s.mutate(w, r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			for _, id := range v.Scope.ServiceIDs {
				var proto string
				if err := tx.QueryRowContext(ctx, "SELECT kind FROM services WHERE id=? AND enabled=1", id).Scan(&proto); err != nil {
					return registry.MutationResult{}, err
				}
				if proto != "tcp-mtls" {
					return registry.MutationResult{}, registry.ErrInvalid
				}
			}
			now := time.Now().UTC()
			exp := now.Add(time.Duration(v.TTL) * time.Second)
			if exp.After(issuer.NotAfter) || now.Before(issuer.NotBefore) {
				return registry.MutationResult{}, registry.ErrInvalid
			}
			serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
			if err != nil {
				return registry.MutationResult{}, err
			}
			id, err := registry.NewID("cert_")
			if err != nil {
				return registry.MutationResult{}, err
			}
			tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: id}, NotBefore: now.Add(-time.Minute), NotAfter: exp, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, csr.PublicKey, signer)
			if err != nil {
				return registry.MutationResult{}, err
			}
			digest := sha256.Sum256(der)
			cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id,principal_id,kind,digest,public_material,scope_json,created_at,expires_at,label) VALUES(?,?,'client_certificate',?,?,?,?,?,?)`, id, p.ID, hex.EncodeToString(digest[:]), cert, string(lifecycle.JSON(v.Scope)), now.UnixMilli(), exp.UnixMilli(), id)
			if err == nil {
				err = registry.AppendEvent(ctx, tx, "auth", "", "", map[string]string{"action": "client_certificate_issued", "credential_id": id}, true)
			}
			return registry.MutationResult{Response: lifecycle.JSON(map[string]any{"certificate_id": id, "certificate_pem": cert, "revision": 1, "expires_at": exp})}, err
		})
	})
}

// StartTCP maintains only explicitly registered, enabled mTLS listeners.
func (s *Server) StartTCP(ctx context.Context) {
	if s.Config.Gateway.ClientCA == "" || s.Config.API.TLSKey == "" {
		return
	}
	crt, err := tls.LoadX509KeyPair(s.Config.API.TLSCert, s.Config.API.TLSKey)
	if err != nil {
		return
	}
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(s.Config.Gateway.ClientCA)
	if err != nil || !pool.AppendCertsFromPEM(ca) {
		return
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{crt}, SessionTicketsDisabled: true}
	listeners := map[string]net.Listener{}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, 128)
	for {
		rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT "+serviceCols+" FROM services WHERE kind='tcp-mtls' AND enabled=1 AND public_port IS NOT NULL")
		if err == nil {
			seen := map[string]bool{}
			for rows.Next() {
				v, err := readService(rows)
				if err != nil {
					continue
				}
				seen[v.ID] = true
				if listeners[v.ID] != nil {
					continue
				}
				host, _, _ := net.SplitHostPort(s.Config.API.Listen)
				l, err := tls.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(v.PublicPort)), cfg)
				if err != nil {
					continue
				}
				listeners[v.ID] = l
				go func() {
					for {
						c, err := l.Accept()
						if err != nil {
							return
						}
						select {
						case slots <- struct{}{}:
							go func() { defer func() { <-slots }(); s.tcpClient(ctx, c.(*tls.Conn), v) }()
						default:
							c.Close()
						}
					}
				}()
			}
			rows.Close()
			for id, l := range listeners {
				if !seen[id] {
					l.Close()
					delete(listeners, id)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Server) certPrincipal(ctx context.Context, der []byte, v service) (auth.Principal, error) {
	digest := sha256.Sum256(der)
	var p auth.Principal
	var raw string
	err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT p.id,c.scope_json FROM credentials c JOIN principals p ON p.id=c.principal_id WHERE p.disabled=0 AND c.kind='client_certificate' AND c.digest=? AND c.revoked_at IS NULL AND c.expires_at>?`, hex.EncodeToString(digest[:]), time.Now().UnixMilli()).Scan(&p.ID, &raw)
	if err != nil {
		return p, auth.ErrUnauthenticated
	}
	var scope certScope
	if json.Unmarshal([]byte(raw), &scope) != nil {
		return p, auth.ErrForbidden
	}
	for _, id := range scope.ServiceIDs {
		if id == v.ID {
			p.Scope = auth.Scope{Access: "tunnel", MachineID: v.MachineID}
			return p, nil
		}
	}
	return p, auth.ErrForbidden
}
func (s *Server) tcpClient(ctx context.Context, c *tls.Conn, v service) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if c.HandshakeContext(ctx) != nil {
		return
	}
	c.SetDeadline(time.Time{})
	cs := c.ConnectionState()
	if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return
	}
	der := cs.PeerCertificates[0].Raw
	p, err := s.certPrincipal(ctx, der, v)
	if err != nil {
		return
	}
	b, check, end, err := s.resolve(ctx, p, v)
	if err != nil {
		return
	}
	defer end()
	backend, err := b.Dial(ctx, func(ctx context.Context) error {
		if _, err := s.certPrincipal(ctx, der, v); err != nil {
			return err
		}
		return check(ctx)
	})
	if err != nil {
		return
	}
	defer backend.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
			backend.Close()
		case <-done:
		}
	}()
	defer close(done)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(backend, c)
		if err != nil {
			backend.Close()
		} else {
			backend.(*net.TCPConn).CloseWrite()
		}
	}()
	_, err = io.Copy(c, backend)
	if err != nil {
		c.Close()
	} else {
		c.CloseWrite()
	}
	wg.Wait()
}
