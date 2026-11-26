package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

const tunnelProtocol = "sandbox.tunnel.v1"

var safeMachineID = regexp.MustCompile(`^m_[A-Za-z0-9_-]+$`)
var safeSSHUser = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c *client) sshConfig() *cobra.Command {
	var user string
	cmd := sub("ssh-config MACHINE", "Print an OpenSSH Host entry", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		id, err := c.machineID(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if !safeMachineID.MatchString(id) || !safeSSHUser.MatchString(user) {
			return errors.New("invalid machine ID or SSH user")
		}
		quote := func(v string) string {
			v = strings.ReplaceAll(v, "%", "%%")
			return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'"
		}
		binary := "sandboxctl"
		if name, err := os.Executable(); err == nil {
			binary = quote(name)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Host sandbox-%s\n  HostName %s\n  User %s\n  HostKeyAlias sandbox-%s\n  UserKnownHostsFile ~/.ssh/sandbox_known_hosts\n  ProxyCommand %s --url %s",
			id, id, user, id, binary, quote(c.origin))
		if c.caFile != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " --ca-file %s", quote(c.caFile))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), " ssh-proxy --machine %s\n", id)
		return err
	})
	cmd.Flags().StringVar(&user, "user", "root", "Guest SSH user")
	cmd.AddCommand(c.sshConfigSync())
	return cmd
}

func (c *client) sshHostKey() *cobra.Command {
	var ifReady bool
	hostKey := sub("ssh-host-key MACHINE", "Print the verified Guest SSH host key for known_hosts", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		id, err := c.machineID(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.printSSHHostKey(cmd, id, ifReady)
	})
	hostKey.Flags().BoolVar(&ifReady, "if-ready", false, "Return no entry if the Guest SSH identity is still unavailable")
	return hostKey
}

func (c *client) printSSHHostKey(cmd *cobra.Command, id string, ifReady bool) error {
	if !safeMachineID.MatchString(id) {
		return errors.New("invalid machine ID")
	}
	var details struct {
		ID      string `json:"machine_id"`
		Binding struct {
			Identity struct {
				Ready bool   `json:"ssh_ready"`
				Key   string `json:"ssh_host_key"`
			} `json:"guest_management_evidence"`
		} `json:"binding"`
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	if err := c.get(ctx, res("machines", id), &details); err != nil {
		return err
	}
	if details.ID != id {
		return errors.New("platform returned a different machine ID")
	}
	host := details.Binding.Identity
	if !host.Ready || host.Key == "" {
		if ifReady {
			return nil
		}
		return errors.New("Guest SSH host key is not verified")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(host.Key))
	if err != nil || len(options) != 0 || len(rest) != 0 {
		return errors.New("platform returned an invalid Guest SSH host key")
	}
	line := "sandbox-" + id + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if c.jsonOutput {
		return c.print(cmd, map[string]string{"known_hosts": line, "fingerprint": ssh.FingerprintSHA256(key)})
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

func (c *client) sshProxy() *cobra.Command {
	var machineID, managedKey string
	var managed bool
	cmd := sub("ssh-proxy", "Proxy OpenSSH over the authenticated platform tunnel", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		if managed && managedKey == "" {
			dir, err := c.profileDirectory()
			if err != nil {
				return err
			}
			managedKey = managedSSHIdentity(filepath.Join(dir, "ssh"))
		}
		if !managed && managedKey != "" {
			return errors.New("--managed-key-file requires --managed")
		}
		return c.proxySSH(cmd, machineID, managedKey)
	})
	cmd.Flags().StringVar(&machineID, "machine", "", "Platform machine ID")
	cmd.Flags().BoolVar(&managed, "managed", false, "Use the managed SSH tunnel for a System Guest")
	cmd.Flags().StringVar(&managedKey, "managed-key-file", "", "Managed key path; defaults to the profile identity")
	_ = cmd.Flags().MarkHidden("managed-key-file")
	_ = cmd.MarkFlagRequired("machine")
	return cmd
}

func (c *client) proxySSH(cmd *cobra.Command, machineID, managedKey string) error {
	if !safeMachineID.MatchString(machineID) {
		return errors.New("invalid machine ID")
	}
	path := res("machines", machineID) + "/ssh-tunnel"
	budget := 15 * time.Second
	var headers http.Header
	if managedKey != "" {
		publicKey, err := managedSSHPublicKey(managedKey)
		if err != nil {
			return err
		}
		headers = http.Header{managedSSHKeyHeader: {publicKey}}
		path += "?managed=1"
		budget = 75 * time.Second
	}
	return c.tunnelWithDialBudget(cmd.Context(), path, headers, cmd.InOrStdin(), cmd.OutOrStdout(), func() error { return nil }, budget)
}

func (c *client) portForward() *cobra.Command {
	var port int
	cmd := sub("port-forward SERVICE", "Forward a local loopback port to an authorized Guest service", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if port < 0 || port > 65535 {
			return errors.New("invalid local port")
		}
		if err := c.setup(); err != nil {
			return err
		}
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return err
		}
		defer listener.Close()
		stop := context.AfterFunc(cmd.Context(), func() { _ = listener.Close() })
		defer stop()
		fmt.Fprintf(cmd.ErrOrStderr(), "Forwarding %s to service %s\n", listener.Addr(), args[0])
		slots := make(chan struct{}, 64)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if cmd.Context().Err() != nil {
					return nil
				}
				return err
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			go func(cn *net.TCPConn) {
				defer cn.Close()
				defer func() { <-slots }()
				if err := c.tunnel(cmd.Context(), res("services", args[0])+"/tunnel", cn, cn, cn.CloseWrite); err != nil && cmd.Context().Err() == nil {
					fmt.Fprintln(cmd.ErrOrStderr(), err)
				}
			}(conn.(*net.TCPConn))
		}
	})
	cmd.Flags().IntVar(&port, "local-port", 8080, "Local TCP port (0 requests an available port)")
	return cmd
}

type tunnelControl struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) tunnel(ctx context.Context, path string, source io.Reader, sink io.Writer, closeWrite func() error) error {
	return c.tunnelWithDialBudget(ctx, path, nil, source, sink, closeWrite, 15*time.Second)
}

func (c *client) tunnelWithDialBudget(ctx context.Context, path string, extraHeaders http.Header, source io.Reader, sink io.Writer, closeWrite func() error, dialBudget time.Duration) error {
	endpoint, err := c.endpoint(path)
	if err != nil {
		return err
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	headers := extraHeaders.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+c.token)
	connect, cancelConnect := context.WithTimeout(ctx, dialBudget)
	ws, response, err := websocket.Dial(connect, endpoint.String(), &websocket.DialOptions{
		HTTPClient: c.http, HTTPHeader: headers, Subprotocols: []string{tunnelProtocol}, CompressionMode: websocket.CompressionDisabled,
	})
	cancelConnect()
	if err != nil {
		if response != nil && response.Body != nil {
			defer response.Body.Close()
		}
		return err
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws.SetReadLimit(1 << 20)
	if ws.Subprotocol() != tunnelProtocol {
		return errors.New("server did not negotiate the tunnel protocol")
	}
	ready, cancelReady := context.WithTimeout(ctx, 2*time.Minute)
	kind, payload, err := ws.Read(ready)
	cancelReady()
	if err != nil {
		return err
	}
	var control tunnelControl
	if kind != websocket.MessageText || json.Unmarshal(payload, &control) != nil {
		return errors.New("tunnel did not return a ready frame")
	}
	if control.Type != "ready" {
		return control.failure()
	}

	done := make(chan error, 2)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := source.Read(buf)
			if n > 0 {
				if writeErr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if err == io.EOF {
				done <- ws.Write(ctx, websocket.MessageText, []byte(`{"type":"close_write"}`))
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		for {
			kind, payload, err := ws.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
					done <- closeWrite()
				} else {
					done <- err
				}
				return
			}
			if kind == websocket.MessageBinary {
				_, err = sink.Write(payload)
				if err != nil {
					done <- err
					return
				}
				continue
			}
			var control tunnelControl
			if json.Unmarshal(payload, &control) == nil && control.Type == "close_write" {
				done <- closeWrite()
			} else {
				done <- control.failure()
			}
			return
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c tunnelControl) failure() error {
	if c.Message != "" {
		return errors.New(c.Message)
	}
	if c.Error.Message != "" {
		return errors.New(c.Error.Message)
	}
	return errors.New("invalid tunnel control frame")
}
