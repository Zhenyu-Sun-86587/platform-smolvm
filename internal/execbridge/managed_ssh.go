package execbridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	rt "sandbox-platform/internal/runtime"
)

const managedSSHChunkSize = 64 << 10
const managedSSHReapWait = 2 * time.Second

// OpenManagedSSHD runs one inetd-mode OpenSSH process on an already scoped
// System Agent connection. The context limits startup. The caller closes the
// returned stream when the authorized tunnel ends.
func OpenManagedSSHD(ctx context.Context, socket net.Conn, publicKey ssh.PublicKey) (net.Conn, error) {
	if publicKey == nil || publicKey.Type() != ssh.KeyAlgoED25519 {
		_ = socket.Close()
		return nil, rt.ErrInvalid
	}
	key, err := ssh.ParsePublicKey(publicKey.Marshal())
	if err != nil || key.Type() != ssh.KeyAlgoED25519 {
		_ = socket.Close()
		return nil, rt.ErrInvalid
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = socket.Close()
		return nil, err
	}
	// Both parts of the key are fixed-format tokens. No shell, client
	// comment, authorization file, or user-controlled option is involved.
	keyCommand := "AuthorizedKeysCommand=/bin/echo restrict,pty,port-forwarding ssh-ed25519 " +
		base64.StdEncoding.EncodeToString(key.Marshal())
	command := []string{
		"/usr/sbin/sshd", "-i", "-e", "-f", "/dev/null", "-h", "/etc/ssh/sandbox-managed/host_ed25519_key",
		"-o", "AllowUsers=root",
		"-o", "PermitRootLogin=prohibit-password",
		"-o", "AuthenticationMethods=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "AuthorizedKeysFile=none",
		"-o", "AuthorizedKeysCommandUser=root",
		"-o", keyCommand,
		"-o", "LoginGraceTime=30",
		"-o", "UsePAM=yes",
		"-o", "PermitTTY=yes",
		"-o", "AllowTcpForwarding=yes",
		"-o", "AllowAgentForwarding=no",
		"-o", "X11Forwarding=no",
		"-o", "PermitUserRC=no",
		"-o", "Subsystem=sftp internal-sftp",
	}
	if err := startGuestCommand(ctx, socket, rt.ExecSessionRequest{
		ID: hex.EncodeToString(random[:]), ExecRequest: rt.ExecRequest{Command: command, Cwd: "/"}, Interactive: true,
	}); err != nil {
		return nil, err
	}
	return &managedSSHConn{Conn: socket}, nil
}

type managedSSHConn struct {
	net.Conn
	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
	readMu    sync.Mutex
	pending   []byte
	readErr   error
	writeMu   sync.Mutex
	writeEOF  bool
}

func (c *managedSSHConn) Read(data []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.closing.Load() {
		return 0, net.ErrClosed
	}
	if len(data) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 && c.readErr == nil {
		response, err := readGuestFrame(c.Conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			c.readErr = err
			break
		}
		switch response.Status {
		case "stdout":
			c.pending = response.Data
		case "stderr", "error":
			// Agent diagnostics are separate from the binary SSH transport.
		case "exited":
			switch {
			case response.ExitCode == nil:
				c.readErr = io.ErrUnexpectedEOF
			case *response.ExitCode == 0:
				c.readErr = io.EOF
			default:
				c.readErr = fmt.Errorf("managed sshd exited with status %d", *response.ExitCode)
			}
		default:
			c.readErr = errors.New("unexpected managed sshd Agent response")
		}
	}
	if c.readErr != nil {
		return 0, c.readErr
	}
	n := copy(data, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *managedSSHConn) Write(data []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closing.Load() || c.writeEOF {
		return 0, net.ErrClosed
	}
	written := 0
	for len(data) != 0 {
		size := min(len(data), managedSSHChunkSize)
		if err := guestStdin(c.Conn, data[:size]); err != nil {
			// A partial framed write cannot be resumed.
			_ = c.Conn.Close()
			return written, err
		}
		written += size
		data = data[size:]
	}
	return written, nil
}

func (c *managedSSHConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closing.Load() {
		return net.ErrClosed
	}
	if c.writeEOF {
		return nil
	}
	c.writeEOF = true
	if err := guestStdin(c.Conn, []byte{}); err != nil {
		_ = c.Conn.Close()
		return err
	}
	return nil
}

func (c *managedSSHConn) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		if socket, ok := c.Conn.(interface{ CloseWrite() error }); ok {
			// Socket EOF cancels the owned child. The Agent kills and waits
			// before emitting Exited; keep the read side only for that receipt.
			timer := time.AfterFunc(managedSSHReapWait, func() { _ = c.Conn.Close() })
			defer timer.Stop()
			if socket.CloseWrite() == nil {
				c.readMu.Lock()
				defer c.readMu.Unlock()
				for c.readErr == nil {
					response, err := readGuestFrame(c.Conn)
					if err != nil || response.Status == "exited" {
						break
					}
				}
			}
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}
