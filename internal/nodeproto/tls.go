package nodeproto

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

func WorkerURI(a Authority) string {
	return "spiffe://sandbox-platform/node/" + a.WorkerID + "/" + strconv.FormatUint(a.EnrollmentGeneration, 10)
}

func ControllerURI(a Authority) string {
	return "spiffe://sandbox-platform/controller/" + a.ControllerID
}

func VerifyPeer(leaf *x509.Certificate, pin, uri string) error {
	if leaf == nil || !ValidDigest(pin) {
		return errors.New("missing enrollment certificate or public key pin")
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare([]byte("sha256:"+hex.EncodeToString(digest[:])), []byte(pin)) != 1 {
		return errors.New("certificate public key differs from enrollment")
	}
	return VerifyCertificateURI(leaf, uri)
}

func VerifyCertificateURI(leaf *x509.Certificate, uri string) error {
	now := time.Now()
	if leaf == nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("enrollment certificate is outside its validity period")
	}
	for _, san := range leaf.URIs {
		if san.String() == uri {
			return nil
		}
	}
	return errors.New("certificate URI differs from enrollment identity")
}

// PeerDeadline is the last expiry of a currently valid verified chain.
func PeerDeadline(chains [][]*x509.Certificate) time.Time {
	now := time.Now()
	var latest time.Time
	for _, chain := range chains {
		var expiry time.Time
		for _, cert := range chain {
			if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				expiry = time.Time{}
				break
			}
			if expiry.IsZero() || cert.NotAfter.Before(expiry) {
				expiry = cert.NotAfter
			}
		}
		if expiry.After(latest) {
			latest = expiry
		}
	}
	return latest
}

func LoadTLS(certificate, key, ca string) (tls.Certificate, *x509.CertPool, error) {
	leaf, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load peer TLS roots: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return tls.Certificate{}, nil, errors.New("peer TLS roots PEM has no certificates")
	}
	return leaf, roots, nil
}
