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
	authority := fixtureSSHSigner(t)
	host := fixtureSSHSigner(t)
	check := ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool {
		return ssh.FingerprintSHA256(key) == ssh.FingerprintSHA256(authority.PublicKey())
	}}
	config := &ssh.ServerConfig{PublicKeyCallback: check.Authenticate}
	config.AddHostKey(host)

	var created, failList, reassigned atomic.Bool
	var issued, tunneled, writes atomic.Int32
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
				"binding": map[string]any{"guest_management_evidence": map[string]any{
					"ssh_ready": true, "ssh_host_key": string(ssh.MarshalAuthorizedKey(host.PublicKey())),
				}},
			})
		case "POST /v1/machines/m_exact/ssh-certificates":
			var request struct {
				Key string `json:"public_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(request.Key))
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			issued.Add(1)
			now := time.Now().Truncate(time.Second)
			expires := now.Add(2 * time.Minute)
			cert := ssh.Certificate{Key: key, CertType: ssh.UserCert, ValidPrincipals: []string{"root"},
				ValidAfter: uint64(now.Add(-time.Second).Unix()), ValidBefore: uint64(expires.Unix())}
			if err := cert.SignCert(rand.Reader, authority); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(certificateReply{string(ssh.MarshalAuthorizedKey(&cert)), expires})
		case "GET /v1/machines/m_exact/ssh-tunnel":
			if r.URL.Query().Get("managed") != "1" {
				t.Error("managed flag missing")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tunneled.Add(1)
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
	cmd := exec.CommandContext(ctx, stockSSH, "-F", sshConfig, "myapp.sandbox", "test")
	cmd.Env = append(os.Environ(), "SANDBOX_URL=http://127.0.0.1:1", "SANDBOX_TOKEN=wrong")
	out, err := cmd.CombinedOutput()
	var remote *exec.ExitError
	if !errors.As(err, &remote) || remote.ExitCode() != 17 || string(out) != "managed\n" {
		t.Fatalf("stock OpenSSH: %v\n%s", err, out)
	}
	if issued.Load() != 1 || tunneled.Load() != 1 {
		t.Fatalf("certificate issuances=%d tunnels=%d", issued.Load(), tunneled.Load())
	}
	direct, err := cli("", "ssh", "myapp", "--", "test")
	if !errors.As(err, &remote) || remote.ExitCode() != 17 || string(direct) != "managed\n" {
		t.Fatalf("direct managed SSH: %v\n%s", err, direct)
	}

	reassigned.Store(true)
	cmd = exec.CommandContext(ctx, stockSSH, "-F", sshConfig, "myapp.sandbox", "test")
	rejected, err := cmd.CombinedOutput()
	if !errors.As(err, &remote) || remote.ExitCode() != 255 || !strings.Contains(string(rejected), "no longer identifies its configured machine") || issued.Load() != 2 || tunneled.Load() != 2 {
		t.Fatalf("stale alias was accepted: err=%v out=%q issued=%d tunneled=%d", err, rejected, issued.Load(), tunneled.Load())
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

func TestLegacySSHFallbackRequiresDefiniteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		effect string
		id     string
		want   bool
	}{
		{"unsupported binding", 409, "NOT_SUPPORTED", "none", "", true},
		{"unknown unsupported result", 409, "NOT_SUPPORTED", "unknown", "", false},
		{"missing old route", 404, "", "", "m_exact", true},
		{"missing old route without same-ID readback", 405, "", "", "m_other", false},
		{"structured missing machine", 404, "NOT_FOUND", "none", "m_exact", false},
		{"unauthorized", 403, "PERMISSION_DENIED", "none", "m_exact", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"machine_id":%q}`, tc.id)
			}))
			defer server.Close()
			c := &client{origin: server.URL, token: "fixture"}
			err := &apiError{Status: tc.status, Detail: apiErrorDetail{Code: tc.code, Effect: tc.effect}}
			if got := c.canUseLegacySSH(context.Background(), "m_exact", err); got != tc.want {
				t.Fatalf("fallback=%v, want %v", got, tc.want)
			}
		})
	}
}
