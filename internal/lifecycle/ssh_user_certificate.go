package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

const managedSSHCertificateTTL = 2 * time.Minute

// ManagedSystemSSHBinding qualifies the platform-managed System SSH profile
// without waking the machine. The tunnel separately acquires an access hold.
func (e *Engine) ManagedSystemSSHBinding(ctx context.Context, mid string) (registry.Binding, error) {
	machine, err := e.DB.GetMachine(ctx, mid)
	if err != nil {
		return registry.Binding{}, err
	}
	if machine.DeletedAt != nil {
		return registry.Binding{}, registry.ErrNotFound
	}
	template, err := e.DB.GetTemplate(ctx, machine.TemplateID)
	if err != nil {
		return registry.Binding{}, err
	}
	profile, err := templates.ReadSpec(template)
	if err != nil {
		return registry.Binding{}, err
	}
	if profile.BootMode != rt.BootSystem || !profile.ManagedSSH {
		return registry.Binding{}, rt.ErrUnsupported
	}
	binding := machine.CurrentBinding
	if binding == nil || binding.Status != "current" || binding.RuntimeInstanceObjectID == "" {
		return registry.Binding{}, registry.ErrPowerState
	}
	if binding.RuntimeBundleID == "" {
		return registry.Binding{}, rt.ErrUnsupported
	}
	adapter, err := e.ForBinding(*binding)
	if err != nil {
		return registry.Binding{}, err
	}
	if _, ok := adapter.(rt.NativeGuestControl); !ok {
		return registry.Binding{}, rt.ErrUnsupported
	}
	return *binding, nil
}

func (e *Engine) managedSSHCA(mid string, create bool) (ssh.Signer, error) {
	if mid == "." || mid == ".." || mid == "" || filepath.Base(mid) != mid {
		return nil, registry.ErrInvalid
	}
	dir := filepath.Join(e.Config.StateDir, "secrets", "ssh-user-ca")
	var raw []byte
	var err error
	if create {
		raw, err = readOrCreateSSHKey(dir, mid)
	} else {
		raw, err = os.ReadFile(filepath.Join(dir, mid))
	}
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(raw)
}

func (e *Engine) systemAuthorizedSSHKeys(ctx context.Context, mid string, createCA bool) ([]byte, error) {
	keys, err := e.authorizedSSHKeys(ctx)
	if err != nil {
		return nil, err
	}
	ca, err := e.managedSSHCA(mid, createCA)
	if err != nil {
		return nil, err
	}
	// This is a shared AuthorizedKeysFile. With no principals option, OpenSSH
	// requires the requested Unix username to appear in the user certificate.
	keys = append(keys, "cert-authority,restrict,pty,port-forwarding "...)
	return append(keys, ssh.MarshalAuthorizedKey(ca.PublicKey())...), nil
}

// SignManagedSSHCertificate signs one key for root in the selected logical
// machine. Authorization and profile qualification precede this call.
func (e *Engine) SignManagedSSHCertificate(mid string, key ssh.PublicKey, source netip.Prefix) (string, time.Time, error) {
	if key == nil || source.Bits() != 32 || !source.Addr().Is4() || !source.Addr().IsPrivate() {
		return "", time.Time{}, registry.ErrInvalid
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return "", time.Time{}, registry.ErrInvalid
	}
	ca, err := e.managedSSHCA(mid, true)
	if err != nil {
		return "", time.Time{}, err
	}
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(managedSSHCertificateTTL)
	cert := &ssh.Certificate{
		Key: key, Serial: binary.BigEndian.Uint64(serial[:]), CertType: ssh.UserCert,
		KeyId: mid, ValidPrincipals: []string{"root"},
		ValidAfter: uint64(now.Unix()), ValidBefore: uint64(expires.Unix()),
		Permissions: ssh.Permissions{
			CriticalOptions: map[string]string{"source-address": source.String()},
			Extensions:      map[string]string{"permit-pty": "", "permit-port-forwarding": ""},
		},
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert))), expires, nil
}
