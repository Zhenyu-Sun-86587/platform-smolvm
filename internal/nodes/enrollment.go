package nodes

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/nodeproto"
)

const WorkerCertificateLifetime = 30 * 24 * time.Hour

type EnrollmentSigner struct {
	ca         *x509.Certificate
	key        crypto.Signer
	host       string
	workerID   string
	generation uint64
	pin        string
}

func PrepareWorkerCSR(encoded, pin string) (*x509.CertificateRequest, [32]byte, error) {
	b, rest := pem.Decode([]byte(encoded))
	if b == nil || b.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, [32]byte{}, errors.New("expected one PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || csr.CheckSignature() != nil || !enrollmentKey(csr.PublicKey) {
		return nil, [32]byte{}, errors.New("invalid signed certificate request")
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(csr.RawSubjectPublicKeyInfo)) != pin {
		return nil, [32]byte{}, errors.New("certificate request key differs from the configured Worker pin")
	}
	return csr, sha256.Sum256(csr.Raw), nil
}

func enrollmentKey(key any) bool {
	switch key := key.(type) {
	case *rsa.PublicKey:
		return key.N.BitLen() >= 2048 && key.N.BitLen() <= 8192
	case *ecdsa.PublicKey:
		return key.Params().BitSize >= 256
	case ed25519.PublicKey:
		return len(key) == ed25519.PublicKeySize
	default:
		return false
	}
}

func LoadEnrollmentSigner(c config.Config, n config.Node) (*EnrollmentSigner, error) {
	if n.CASigningKey == "" {
		return nil, errors.New("Worker enrollment signing is disabled")
	}
	if n.CASigningKey == c.API.TLSKey || n.CASigningKey == c.Gateway.ClientCAKey {
		return nil, errors.New("Worker enrollment cannot reuse another platform key")
	}
	configured, err := url.Parse(n.Endpoint)
	if err != nil || configured.Scheme != "https" || configured.Hostname() == "" {
		return nil, errors.New("invalid Worker certificate endpoint")
	}
	host := configured.Hostname()
	if net.ParseIP(host) == nil && strings.Trim(host, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-") != "" {
		return nil, errors.New("Worker certificate requires a DNS name or IP literal")
	}
	roots, err := enrollmentCertificates(n.CA)
	if err != nil || len(roots) != 1 {
		return nil, errors.New("Worker enrollment requires one dedicated signing CA certificate")
	}
	ca := roots[0]
	now := time.Now()
	if !ca.BasicConstraintsValid || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 ||
		!enrollmentKey(ca.PublicKey) || ca.CheckSignatureFrom(ca) != nil || !bytes.Equal(ca.RawIssuer, ca.RawSubject) ||
		now.Before(ca.NotBefore) || !ca.NotAfter.After(now.Add(enrollmentCAMinimumLifetime)) {
		return nil, errors.New("Worker enrollment signer must be a current self-signed CA")
	}
	paths := []string{c.API.TLSCert, c.Gateway.ClientCA}
	for _, worker := range c.Nodes {
		paths = append(paths, worker.Certificate)
		if worker.ID != n.ID {
			paths = append(paths, worker.CA)
		}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		certificates, err := enrollmentCertificates(path)
		if err != nil {
			return nil, errors.New("cannot verify Worker enrollment CA isolation")
		}
		for _, cert := range certificates {
			if bytes.Equal(cert.RawSubjectPublicKeyInfo, ca.RawSubjectPublicKeyInfo) {
				return nil, errors.New("Worker enrollment CA key is shared with another trust domain")
			}
		}
	}
	secret, err := readEnrollmentPEM(n.CASigningKey, true)
	if err != nil {
		return nil, errors.New("cannot load private Worker enrollment key")
	}
	defer clear(secret)
	block, extra := pem.Decode(secret)
	if block == nil || len(bytes.TrimSpace(extra)) != 0 {
		return nil, errors.New("expected one Worker enrollment private key")
	}
	defer clear(block.Bytes)
	var key any
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		err = errors.New("unsupported private key format")
	}
	signer, ok := key.(crypto.Signer)
	if err != nil || !ok {
		return nil, errors.New("invalid Worker enrollment private key")
	}
	spki, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(spki, ca.RawSubjectPublicKeyInfo) {
		return nil, errors.New("Worker enrollment private key does not match its dedicated CA")
	}
	return &EnrollmentSigner{ca: ca, key: signer, host: host, workerID: n.ID, generation: n.Generation, pin: n.Pin}, nil
}

const enrollmentCAMinimumLifetime = 10 * time.Minute

func (s *EnrollmentSigner) Sign(csr *x509.CertificateRequest, authority nodeproto.Authority) ([]byte, error) {
	identity, err := url.Parse(nodeproto.WorkerURI(authority))
	if err != nil || !authority.Valid() || authority.WorkerID != s.workerID || authority.EnrollmentGeneration != s.generation {
		return nil, errors.New("invalid Worker enrollment identity")
	}
	public, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(public)) != s.pin {
		return nil, errors.New("Worker signing key scope differs from the configured pin")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
	if err != nil {
		return nil, err
	}
	serial.Add(serial, big.NewInt(1))
	now := time.Now().UTC()
	notBefore := now.Add(-time.Minute)
	if s.ca.NotBefore.After(notBefore) {
		notBefore = s.ca.NotBefore
	}
	notAfter := now.Add(WorkerCertificateLifetime)
	if s.ca.NotAfter.Before(notAfter) {
		notAfter = s.ca.NotAfter
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: notBefore, NotAfter: notAfter,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs: []*url.URL{identity}}
	if ip := net.ParseIP(s.host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{s.host}
	}
	return x509.CreateCertificate(rand.Reader, leaf, s.ca, csr.PublicKey, s.key)
}

func enrollmentCertificates(path string) ([]*x509.Certificate, error) {
	raw, err := readEnrollmentPEM(path, false)
	if err != nil {
		return nil, err
	}
	var certificates []*x509.Certificate
	for len(bytes.TrimSpace(raw)) != 0 {
		b, rest := pem.Decode(raw)
		if b == nil || b.Type != "CERTIFICATE" {
			return nil, errors.New("invalid Worker enrollment CA PEM")
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, cert)
		raw = rest
	}
	if len(certificates) == 0 {
		return nil, errors.New("empty Worker enrollment CA PEM")
	}
	return certificates, nil
}

func readEnrollmentPEM(path string, private bool) ([]byte, error) {
	const limit = 128 << 10
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || private && info.Mode().Perm()&0027 != 0 {
		return nil, errors.New("invalid Worker enrollment PEM file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(raw) > limit {
		return nil, errors.New("Worker enrollment PEM exceeds its limit")
	}
	return raw, nil
}
