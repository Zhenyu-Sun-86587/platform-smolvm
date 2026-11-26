package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const managedSSHKeyHeader = "X-Sandbox-SSH-Public-Key"

func managedSSHIdentity(dir string) string {
	return filepath.Join(dir, "ssh-ed25519")
}

func ensureManagedSSHIdentity(ctx context.Context, dir string) (string, error) {
	if err := ensurePrivateDirectory(dir); err != nil {
		return "", err
	}
	lock, err := sshFileLock(ctx, filepath.Join(dir, ".credentials.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	path := managedSSHIdentity(dir)
	if _, err := loadOrGenerateSSHKey(path); err != nil {
		return "", err
	}
	return path, nil
}

func readManagedSSHKey(path string) (ssh.Signer, error) {
	raw, err := readPrivate(path, 16<<10)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("saved managed SSH key is not an unencrypted Ed25519 key")
	}
	return signer, nil
}

func managedSSHPublicKey(path string) (string, error) {
	signer, err := readManagedSSHKey(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

func loadOrGenerateSSHKey(path string) (ssh.Signer, error) {
	signer, err := readManagedSSHKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return signer, err
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
