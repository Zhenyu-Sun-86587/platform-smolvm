package platformapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

func TestWorkerEnrollmentTicketTLSRotationAndRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := registry.Open(ctx, filepath.Join(dir, "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	control, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Worker-specific issuer"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	caPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	for path, block := range map[string]*pem.Block{caPath: {Type: "CERTIFICATE", Bytes: caDER}, keyPath: {Type: "PRIVATE KEY", Bytes: pk}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	csrKey, pin := enrollmentTestKey(t)
	csr := enrollmentTestCSR(t, csrKey, "untrusted-csr-subject")
	wrongKey, _ := enrollmentTestKey(t)
	const worker, endpoint = "worker-a", "https://worker-a.example.test:9443"
	conf := config.Defaults()
	conf.Nodes = []config.Node{{ID: worker, Endpoint: endpoint, Generation: 1, Pin: pin, CA: caPath, CASigningKey: keyPath},
		{ID: "worker-presigned", Endpoint: "https://worker-presigned.example.test", Generation: 1}}
	if err := db.RegisterWorker(ctx, registry.Worker{ID: worker, Name: worker, Endpoint: endpoint}); err != nil {
		t.Fatal(err)
	}
	server := New(lifecycle.New(db, rt.NewResolver(nil, nil), conf), &snapshots.Catalog{DB: db}, nil)
	admin, err := server.Auth.Bootstrap(ctx, "owner", "bounded-test-password")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, adminToken, err := server.Auth.IssueTokenTx(ctx, tx, admin, "admin", auth.Scope{Access: auth.AccessManage}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, readToken, err := server.Auth.IssueTokenTx(ctx, tx, admin, "read", auth.Scope{Access: auth.AccessRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	const issue, redeem = "/v1/workers/worker-a/enrollment/ticket", "/v1/workers/worker-a/enrollment/redeem"
	call := func(path, credential, body, scheme, epoch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, scheme+"://"+server.ManagementHost()+path, strings.NewReader(body))
		r.Header.Set("Authorization", credential)
		r.Header.Set("If-Control-Epoch", epoch)
		out := httptest.NewRecorder()
		server.ServeHTTP(out, r)
		return out
	}
	issueWith := func(path, credential, mode, scheme, epoch string, status int) string {
		t.Helper()
		r := call(path, "Bearer "+credential, `{"mode":"`+mode+`"}`, scheme, epoch)
		if r.Code != status {
			t.Fatalf("issue returned %d, want %d", r.Code, status)
		}
		if status != 201 {
			return ""
		}
		var reply struct {
			Ticket    string    `json:"ticket"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &reply); err != nil || len(reply.Ticket) != 43 ||
			time.Until(reply.ExpiresAt) <= 0 || time.Until(reply.ExpiresAt) > registry.WorkerEnrollmentTicketLifetime {
			t.Fatal("invalid short enrollment ticket receipt")
		}
		return reply.Ticket
	}
	redeemWith := func(path, ticket, request, scheme string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"csr_pem": request})
		return call(path, "WorkerEnrollment "+ticket, string(body), scheme, "")
	}
	assertStatus := func(http *httptest.ResponseRecorder, status int) {
		t.Helper()
		if http.Code != status {
			t.Fatalf("redeem returned %d, want %d", http.Code, status)
		}
	}
	issueWith(issue, readToken, "initial", "https", control.ControlEpoch, 403)
	issueWith(issue, adminToken, "initial", "http", control.ControlEpoch, 403)
	issueWith(issue, adminToken, "initial", "https", "stale-epoch", 409)
	issueWith(issue, adminToken, "rotate", "https", control.ControlEpoch, 409)
	issueWith("/v1/workers/worker-presigned/enrollment/ticket", adminToken, "initial", "https", control.ControlEpoch, 409)

	replaced := issueWith(issue, adminToken, "initial", "https", control.ControlEpoch, 201)
	ticket := issueWith(issue, adminToken, "initial", "https", control.ControlEpoch, 201)
	assertStatus(redeemWith(redeem, replaced, csr, "https"), 401)
	assertStatus(redeemWith("/v1/workers/another/enrollment/redeem", ticket, csr, "https"), 401)
	assertStatus(redeemWith(redeem, ticket, csr, "http"), 401)
	assertStatus(redeemWith(redeem, ticket, enrollmentTestCSR(t, wrongKey, ""), "https"), 400)
	assertStatus(redeemWith(redeem, ticket, "corrupt-csr", "https"), 400)

	var results [2]*httptest.ResponseRecorder
	var sends sync.WaitGroup
	for i := range results {
		sends.Go(func() { results[i] = redeemWith(redeem, ticket, csr, "https") })
	}
	sends.Wait()
	for _, result := range results {
		assertStatus(result, 200)
	}
	if !bytes.Equal(results[0].Body.Bytes(), results[1].Body.Bytes()) {
		t.Fatal("concurrent redemption did not return one stored certificate")
	}
	assertStatus(redeemWith(redeem, ticket, enrollmentTestCSR(t, csrKey, "changed-CSR"), "https"), 401)
	issueWith(issue, adminToken, "initial", "https", control.ControlEpoch, 409)
	var response struct {
		nodeproto.Authority
		Certificate string `json:"certificate_pem"`
	}
	if err := json.Unmarshal(results[0].Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(response.Certificate))
	if block == nil {
		t.Fatal("certificate not returned")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "worker-a.example.test"}); err != nil {
		t.Fatal(err)
	}
	if cert.IsCA || cert.Subject.CommonName != "" || len(cert.DNSNames) != 1 || len(cert.IPAddresses) != 0 ||
		len(cert.URIs) != 1 || len(cert.EmailAddresses) != 0 || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth ||
		response.Authority.WorkerID != worker || response.Authority.EnrollmentGeneration != 1 ||
		nodeproto.VerifyPeer(cert, pin, nodeproto.WorkerURI(response.Authority)) != nil {
		t.Fatal("certificate copied unauthorized CSR identity or extensions")
	}
	var stored []byte
	rawSecret, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(rawSecret)
	if err := db.ReadDB().QueryRow("SELECT ticket_hash FROM worker_enrollment_tickets WHERE worker_id=?", worker).Scan(&stored); err != nil || !bytes.Equal(stored, expected[:]) {
		t.Fatal("raw enrollment credentials retained instead of hashed")
	}
	if _, err := db.GetWorkerEnrollment(ctx, worker); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("signed leaf became active without authenticated Node: %v", err)
	}

	if err := db.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: worker, Generation: 1, Authority: control.ControlEpoch, CertificateFingerprint: pin}); err != nil {
		t.Fatal(err)
	}
	server.Config.Nodes[0].Generation = 2
	assertStatus(redeemWith(redeem, ticket, csr, "https"), 401)
	rotating := issueWith(issue, adminToken, "rotate", "https", control.ControlEpoch, 201)
	state, err := db.GetWorker(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RevokeWorkerEnrollmentTx(ctx, tx, worker, state.Revision, 1, "revoke while ticket issued"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertStatus(redeemWith(redeem, rotating, csr, "https"), 401)
	fresh := issueWith(issue, adminToken, "rotate", "https", control.ControlEpoch, 201)
	if _, err := db.DB().Exec("UPDATE worker_enrollment_tickets SET expires_at=1"); err != nil {
		t.Fatal(err)
	}
	assertStatus(redeemWith(redeem, fresh, csr, "https"), 401)
	fresh = issueWith(issue, adminToken, "rotate", "https", control.ControlEpoch, 201)
	if _, err := db.DB().Exec("UPDATE meta SET control_epoch='new-epoch' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	assertStatus(redeemWith(redeem, fresh, csr, "https"), 401)
}

func enrollmentTestKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key, fmt.Sprintf("sha256:%x", sha256.Sum256(spki))
}

func enrollmentTestCSR(t *testing.T, key ed25519.PrivateKey, subject string) string {
	t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: subject},
		DNSNames: []string{"untrusted.example.test", "*.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
}
