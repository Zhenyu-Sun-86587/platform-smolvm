package lifecycle

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

// Keys belong to a logical machine. Its current and replacement binding share
// an identity only after the lifecycle operation has fenced the old execution.
func (e *Engine) SSHHostKey(mid string) ([]byte, []byte, error) {
	dir := filepath.Join(e.Config.StateDir, "secrets", "ssh")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, mid)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, mid)
		if err != nil {
			return nil, nil, err
		}
		raw = pem.EncodeToMemory(block)
		f, err := os.CreateTemp(dir, ".new-")
		if err != nil {
			return nil, nil, err
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(raw); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		err = os.Link(f.Name(), path)
		if errors.Is(err, os.ErrExist) {
			raw, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, nil, err
		}
		d, err := os.Open(dir)
		if err != nil {
			return nil, nil, err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}
func (e *Engine) InstallSSH(ctx context.Context, b registry.Binding) (string, error) {
	private, public, err := e.SSHHostKey(b.MachineID)
	if err != nil {
		return "", err
	}
	keys, err := e.DB.ReadDB().QueryContext(ctx, "SELECT public_material FROM credentials WHERE kind='ssh_public_key' AND revoked_at IS NULL ORDER BY id")
	if err != nil {
		return "", err
	}
	var authorized bytes.Buffer
	for keys.Next() {
		var key string
		if err = keys.Scan(&key); err != nil {
			keys.Close()
			return "", err
		}
		authorized.WriteString(strings.TrimSpace(key))
		authorized.WriteByte('\n')
	}
	err = keys.Err()
	keys.Close()
	if err != nil {
		return "", err
	}
	mode := uint32(0600)
	for _, v := range []struct {
		name string
		data []byte
	}{{"/run/sandbox-taskd/host-key.new", private}, {"/run/sandbox-taskd/host-pub.new", public}, {"/run/sandbox-taskd/authorized-keys.new", authorized.Bytes()}} {
		if err = e.Runtime.UploadFileWithOptions(ctx, b.RuntimeName, v.name, bytes.NewReader(v.data), rt.FileOptions{Size: int64(len(v.data)), Mode: &mode}); err != nil {
			return "", err
		}
	}
	// This fixed command only addresses paths within the selected guest. Never
	// interpolate supplied user keys or a host path into guest shell source.
	const install = `set -eu
 umask 077
 d=/run/sandbox-taskd
 svc=/usr/local/share/sandbox-managed/services/sshd
 install -d -m 0700 /root/.ssh
 install -m 0600 "$d/authorized-keys.new" /root/.ssh/authorized_keys
 if ! cmp -s "$d/host-key.new" /etc/ssh/sandbox_host_ed25519_key; then
   /usr/bin/sv -w 10 force-stop "$svc"
   install -m 0600 "$d/host-key.new" /etc/ssh/sandbox_host_ed25519_key
   install -m 0644 "$d/host-pub.new" /etc/ssh/sandbox_host_ed25519_key.pub
   sync -f /etc/ssh
   /usr/bin/sv -w 10 start "$svc"
 fi
 rm -f "$d/host-key.new" "$d/host-pub.new" "$d/authorized-keys.new"
 ssh-keygen -y -f /etc/ssh/sandbox_host_ed25519_key`
	res, err := e.Runtime.Exec(ctx, b.RuntimeName, rt.ExecRequest{Command: []string{"/bin/sh", "-c", install}, TimeoutSeconds: 30})
	if err != nil {
		return "", err
	}
	a, _, _, _, err := ssh.ParseAuthorizedKey(res.Stdout)
	if res.ExitCode != 0 || err != nil || !bytes.HasPrefix(public, bytes.TrimSpace(ssh.MarshalAuthorizedKey(a))) {
		return "", errors.New("guest SSH host key installation not verified")
	}
	return string(bytes.TrimSpace(public)), nil
}
