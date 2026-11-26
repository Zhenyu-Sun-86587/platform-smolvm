package platformapi

import (
	"crypto/rsa"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
)

const managedSSHCertificateRequestLimit = 8 << 10

func managedSSHPublicKey(text string) (ssh.PublicKey, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n\x00") {
		return nil, registry.ErrInvalid
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(options) != 0 || len(rest) != 0 {
		return nil, registry.ErrInvalid
	}
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return key, nil
	case ssh.KeyAlgoRSA:
		if pub, ok := key.(ssh.CryptoPublicKey).CryptoPublicKey().(*rsa.PublicKey); ok && pub.N.BitLen() >= 2048 && pub.N.BitLen() <= 8192 {
			return key, nil
		}
	}
	return nil, registry.ErrInvalid
}

func (s *Server) issueManagedSSHCertificate(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	mid := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, mid); err != nil {
		return err
	}
	var request struct {
		PublicKey string `json:"public_key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, managedSSHCertificateRequestLimit)
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return registry.ErrInvalid
		}
		return err
	}
	key, err := managedSSHPublicKey(request.PublicKey)
	if err != nil {
		return err
	}
	if _, err := s.ManagedSystemSSHBinding(r.Context(), mid); err != nil {
		return err
	}
	if err := s.CheckAccessPrincipal(r.Context(), p, mid); err != nil {
		return err
	}
	certificate, expires, err := s.SignManagedSSHCertificate(mid, key)
	if err != nil {
		return err
	}
	return write(w, http.StatusOK, struct {
		Certificate string    `json:"certificate"`
		ExpiresAt   time.Time `json:"expires_at"`
	}{Certificate: certificate, ExpiresAt: expires})
}
