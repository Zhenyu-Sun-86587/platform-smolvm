package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

func TestMain(m *testing.M) {
	if os.Getenv("SANDBOXCTL_MANAGED_SSH_TEST_PROCESS") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixtureSSHSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func serveManagedFixtureSSH(w http.ResponseWriter, r *http.Request, config *ssh.ServerConfig) error {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{tunnelProtocol}})
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	if err := ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
		return err
	}
	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	defer conn.Close()
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(requests)
	incoming, ok := <-channels
	if !ok || incoming.ChannelType() != "session" {
		return errors.New("client did not open an SSH session")
	}
	channel, session, err := incoming.Accept()
	if err != nil {
		return err
	}
	defer channel.Close()
	req, ok := <-session
	if !ok || req.Type != "exec" {
		return errors.New("client did not submit an SSH command")
	}
	if err := req.Reply(true, nil); err != nil {
		return err
	}
	if _, err := io.WriteString(channel, "managed\n"); err != nil {
		return err
	}
	_, err = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{17}))
	return err
}

func TestManagedOpenSSHAliasAfterCreate(t *testing.T) {
	stockSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("stock OpenSSH not installed")
	}
	const token = "managed-ssh-fixture"
	t.Setenv("SANDBOX_URL", "")
	t.Setenv("SANDBOX_TOKEN", "")
	t.Setenv("SANDBOX_CA_FILE", "")
	t.Setenv("SANDBOX_PROFILE", "")
	t.Setenv("SANDBOXCTL_MANAGED_SSH_TEST_PROCESS", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	profile := filepath.Join(root, "local profile")
	sshConfig := filepath.Join(root, "my ssh", "config")
	if err := os.Mkdir(filepath.Dir(sshConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshConfig, []byte("# existing settings\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDBOXCTL_CONFIG_DIR", profile)
	host := fixtureSSHSigner(t)

	var created, failList, reassigned atomic.Bool
	var tunneled, writes, inventoryReads atomic.Int32
	var firstPublicKey atomic.Pointer[string]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("wrong credentials at %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/control":
			_, _ = io.WriteString(w, `{"control_epoch":"epoch"}`)
		case "GET /v1/machines":
			inventoryReads.Add(1)
			if failList.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			items := `[]`
			if created.Load() {
				id := "m_exact"
				if reassigned.Load() {
					id = "m_other"
				}
				items = fmt.Sprintf(`[{"machine_id":%q,"boot_mode":"system","name":"myapp","capabilities":{"access.managed_ssh":{"status":"supported","constraints":{"manage_allowed":true}}}}]`, id)
			}
			_, _ = fmt.Fprintf(w, `{"items":%s}`, items)
		case "POST /v1/machines":
			writes.Add(1)
			created.Store(true)
			_, _ = io.WriteString(w, `{"kind":"create","operation_id":"op_create","state":"succeeded","effect":"applied"}`)
		case "GET /v1/operations/op_create":
			_, _ = io.WriteString(w, `{"kind":"create","operation_id":"op_create","state":"succeeded","effect":"applied"}`)
		case "GET /v1/machines/m_exact":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machine_id": "m_exact",
				"capabilities": map[string]any{managedSSHCapabilityName: map[string]any{
					"status": "supported", "constraints": map[string]bool{"manage_allowed": true},
				}},
				"binding": map[string]any{"guest_management_evidence": map[string]any{
					"ssh_ready": true, "ssh_host_key": string(ssh.MarshalAuthorizedKey(host.PublicKey())),
				}},
			})
		case "GET /v1/machines/m_exact/ssh-tunnel":
			values := r.Header.Values(managedSSHKeyHeader)
			if r.URL.Query().Get("managed") != "1" || len(values) != 1 {
				t.Error("managed flag or public key header missing")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(values[0]))
			if err != nil || key.Type() != ssh.KeyAlgoED25519 || comment != "" || len(options) != 0 || len(rest) != 0 ||
				values[0] != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) {
				t.Errorf("invalid managed public key header: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !firstPublicKey.CompareAndSwap(nil, &values[0]) && *firstPublicKey.Load() != values[0] {
				t.Error("direct SSH and installed aliases did not share the saved profile identity")
			}
			tunneled.Add(1)
			config := &ssh.ServerConfig{PublicKeyCallback: func(conn ssh.ConnMetadata, presented ssh.PublicKey) (*ssh.Permissions, error) {
				if conn.User() != "root" || ssh.FingerprintSHA256(key) != ssh.FingerprintSHA256(presented) {
					return nil, errors.New("wrong key for this tunnel")
				}
				return nil, nil
			}}
			config.AddHostKey(host)
			if err := serveManagedFixtureSSH(w, r, config); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cli := func(stdin string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.CombinedOutput()
	}
	if out, err := cli(token+"\n", "login", "--url", server.URL, "--token-stdin", "--ssh-config", sshConfig); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err := cli("", "--json", "machines", "create", "myapp", "--template", "system"); err != nil || !json.Valid(out) {
		t.Fatalf("create: %v\n%s", err, out)
	}
	checkData, err := os.ReadFile(sshConfig)
	if err != nil || strings.Contains(string(checkData), token) || !strings.Contains(string(checkData), "# existing settings\n") {
		t.Fatalf("invalid OpenSSH Include: %v\n%s", err, checkData)
	}
	path := filepath.Join(profile, "managed-ssh.conf")
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), token) || strings.Contains(string(data), "CertificateFile") || strings.Contains(string(data), "Match ") {
		t.Fatalf("invalid managed SSH config: %v\n%s", err, data)
	}
	keyFiles, err := os.ReadDir(filepath.Join(profile, "profiles", "default", "ssh"))
	if err != nil || len(keyFiles) != 2 || keyFiles[0].Name() != ".credentials.lock" || keyFiles[1].Name() != "ssh-ed25519" {
		t.Fatalf("unexpected profile identity files: %v, %v", keyFiles, err)
	}
	reads := inventoryReads.Load()
	check := exec.CommandContext(ctx, stockSSH, "-G", "-F", sshConfig, "myapp.sandbox")
	if out, err := check.Output(); err != nil || !strings.Contains(string(out), "hostkeyalias sandbox-m_exact") || inventoryReads.Load() != reads {
		t.Fatalf("ssh -G did not resolve passively: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, stockSSH, "-F", sshConfig, "myapp.sandbox", "test")
	cmd.Env = append(os.Environ(), "SANDBOX_URL=http://127.0.0.1:1", "SANDBOX_TOKEN=wrong")
	out, err := cmd.CombinedOutput()
	var remote *exec.ExitError
	if !errors.As(err, &remote) || remote.ExitCode() != 17 || string(out) != "managed\n" {
		t.Fatalf("stock OpenSSH: %v\n%s", err, out)
	}
	if tunneled.Load() != 1 {
		t.Fatalf("managed tunnels=%d", tunneled.Load())
	}
	direct, err := cli("", "ssh", "myapp", "--", "test")
	if !errors.As(err, &remote) || remote.ExitCode() != 17 || string(direct) != "managed\n" {
		t.Fatalf("direct managed SSH: %v\n%s", err, direct)
	}

	reassigned.Store(true)
	cmd = exec.CommandContext(ctx, stockSSH, "-F", sshConfig, "myapp.sandbox", "test")
	rejected, err := cmd.CombinedOutput()
	if !errors.As(err, &remote) || remote.ExitCode() != 255 || !strings.Contains(string(rejected), "no longer identifies its configured machine") || tunneled.Load() != 2 {
		t.Fatalf("stale alias was accepted: err=%v out=%q tunneled=%d", err, rejected, tunneled.Load())
	}
	failList.Store(true)
	cmd = exec.CommandContext(ctx, binary, "--json", "operations", "wait", "op_create")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err != nil || !json.Valid(out) || !json.Valid([]byte(stderr.String())) || !strings.Contains(stderr.String(), "warning") || writes.Load() != 1 {
		t.Fatalf("failed alias refresh altered the receipt: err=%v stdout=%q stderr=%q creates=%d", err, out, stderr.String(), writes.Load())
	}
}

func TestExplicitTokenDoesNotAdoptProfileOrigin(t *testing.T) {
	c := &client{origin: "https://127.0.0.1:8443", token: "explicit", useProfiles: true, profileName: "default", configDir: t.TempDir()}
	profile, err := c.profileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(profile); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(profile, "profile.json"), []byte(`{"origin":"https://saved.example","token":"saved"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	if c.base.Host != "127.0.0.1:8443" || c.token != "explicit" {
		t.Fatalf("explicit token directed to %s", c.base)
	}
}

func TestManagedSSHSelectionRequiresDefiniteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		status           int
		managed, failure bool
	}{
		{"supported", `{"machine_id":"m_exact","capabilities":{"access.managed_ssh":{"status":"supported","constraints":{"manage_allowed":true}}}}`, 200, true, false},
		{"unsupported", `{"machine_id":"m_exact","capabilities":{"access.managed_ssh":{"status":"unsupported","constraints":{"manage_allowed":true}}}}`, 200, false, false},
		{"older machine view", `{"machine_id":"m_exact"}`, 200, false, false},
		{"unexpected machine", `{"machine_id":"m_other"}`, 200, false, true},
		{"support unknown", `{"machine_id":"m_exact","capabilities":{"access.managed_ssh":{"status":"unknown","constraints":{"manage_allowed":true}}}}`, 200, false, true},
		{"permission unknown", `{"machine_id":"m_exact","capabilities":{"access.managed_ssh":{"status":"unsupported"}}}`, 200, false, true},
		{"permission denied", `{"machine_id":"m_exact","capabilities":{"access.managed_ssh":{"status":"unsupported","constraints":{"manage_allowed":false}}}}`, 200, false, true},
		{"unauthorized request", `{"error":{"code":"PERMISSION_DENIED","effect":"none"}}`, 403, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/machines/m_exact" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Errorf("unexpected machine read: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := &client{origin: server.URL, token: "fixture"}
			managed, err := c.machineManagedSSH(context.Background(), "m_exact")
			if managed != tc.managed || (err != nil) != tc.failure {
				t.Fatalf("managed=%v, error=%v", managed, err)
			}
		})
	}
}
