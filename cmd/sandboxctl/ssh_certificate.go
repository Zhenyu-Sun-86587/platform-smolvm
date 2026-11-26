package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const sshCredentialRefreshLead = 25 * time.Second

type sshCredentialPaths struct {
	identity, certificate string
}

func credentialPaths(dir, machine string) sshCredentialPaths {
	return sshCredentialPaths{
		identity:    filepath.Join(dir, "ssh-ed25519"),
		certificate: filepath.Join(dir, machine+"-cert.pub"),
	}
}

func (c *client) prepareSSHCredential(ctx context.Context, dir, machine string) (sshCredentialPaths, error) {
	if !safeMachineID.MatchString(machine) {
		return sshCredentialPaths{}, errors.New("invalid machine ID for managed SSH")
	}
	if err := ensurePrivateDirectory(dir); err != nil {
		return sshCredentialPaths{}, err
	}
	lock, err := sshFileLock(ctx, filepath.Join(dir, ".credentials.lock"))
	if err != nil {
		return sshCredentialPaths{}, err
	}
	defer lock.Close()
	paths := credentialPaths(dir, machine)
	signer, err := loadOrGenerateSSHKey(paths.identity)
	if err != nil {
		return sshCredentialPaths{}, err
	}
	if data, err := readPrivate(paths.certificate, 32<<10); err == nil {
		if _, err := validateSSHCertificate(data, signer.PublicKey(), time.Now(), sshCredentialRefreshLead); err == nil {
			return paths, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return sshCredentialPaths{}, err
	}

	response, err := c.issueSSHCertificate(ctx, machine, signer.PublicKey())
	if err != nil {
		return sshCredentialPaths{}, fmt.Errorf("obtain managed SSH certificate (use --legacy or --identity for enrolled keys): %w", err)
	}
	certificate, err := validateSSHCertificate([]byte(response.Certificate), signer.PublicKey(), time.Now(), 0)
	if err != nil {
		return sshCredentialPaths{}, err
	}
	if !response.ExpiresAt.Equal(time.Unix(int64(certificate.ValidBefore), 0)) {
		return sshCredentialPaths{}, errors.New("managed SSH certificate and receipt expiration differ")
	}
	if err := writePrivate(paths.certificate, ssh.MarshalAuthorizedKey(certificate)); err != nil {
		return sshCredentialPaths{}, err
	}
	return paths, nil
}

type certificateReply struct {
	Certificate string    `json:"certificate"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (c *client) issueSSHCertificate(ctx context.Context, machine string, key ssh.PublicKey) (certificateReply, error) {
	body, err := json.Marshal(map[string]string{"public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))})
	if err != nil {
		return certificateReply{}, err
	}
	resp, err := c.request(ctx, http.MethodPost, res("machines", machine)+"/ssh-certificates", bytes.NewReader(body),
		http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return certificateReply{}, err
	}
	defer resp.Body.Close()
	var reply certificateReply
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&reply)
	return reply, err
}

func loadOrGenerateSSHKey(path string) (ssh.Signer, error) {
	raw, err := readPrivate(path, 16<<10)
	switch {
	case err == nil:
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return nil, errors.New("saved managed SSH key is not an unencrypted Ed25519 key")
		}
		return signer, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "sandboxctl-managed")
	if err != nil {
		return nil, err
	}
	if err := writePrivate(path, pem.EncodeToMemory(block)); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(private)
}

func validateSSHCertificate(raw []byte, key ssh.PublicKey, now time.Time, lifetime time.Duration) (*ssh.Certificate, error) {
	parsed, _, options, rest, err := ssh.ParseAuthorizedKey(raw)
	cert, ok := parsed.(*ssh.Certificate)
	if err != nil || len(options) != 0 || len(rest) != 0 || !ok || cert.CertType != ssh.UserCert ||
		!bytes.Equal(cert.Key.Marshal(), key.Marshal()) || len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "root" ||
		cert.ValidAfter > uint64(now.Add(5*time.Second).Unix()) || cert.ValidBefore <= uint64(now.Add(lifetime).Unix()) ||
		cert.ValidBefore > uint64(now.Add(3*time.Minute).Unix()) {
		return nil, errors.New("invalid or expired managed SSH user certificate")
	}
	return cert, nil
}

func sshFileLock(ctx context.Context, path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
