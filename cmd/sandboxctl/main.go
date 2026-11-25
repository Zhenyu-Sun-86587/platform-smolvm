package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"sandbox-platform/internal/buildinfo"
)

type client struct {
	origin, token, caFile string
	jsonOutput, noWait    bool
	waitTimeout           time.Duration
	epoch, key            string
	revision, generation  int64
	http                  *http.Client
	base                  *url.URL
}

type apiError struct {
	Status int
	Detail apiErrorDetail
}

func (e *apiError) Error() string {
	s := e.Detail.Message
	if s == "" {
		s = http.StatusText(e.Status)
	}
	if e.Detail.Code != "" {
		s = e.Detail.Code + ": " + s
	}
	if e.Detail.OperationID != "" {
		s += " (operation " + e.Detail.OperationID + ")"
	}
	return s
}

type operation struct {
	ID     string          `json:"operation_id"`
	State  string          `json:"state"`
	Kind   string          `json:"kind"`
	Stage  string          `json:"stage"`
	Effect string          `json:"effect"`
	Result json.RawMessage `json:"result"`
}

func (o operation) terminal() bool {
	switch o.State {
	case "succeeded", "failed", "cancelled", "recovery_required":
		return true
	default:
		return false
	}
}

type machine struct {
	ID         string `json:"machine_id"`
	Revision   int64  `json:"revision"`
	Generation int64  `json:"binding_generation"`
}

type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", e) }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root, cli := rootCommand()
	if err := root.ExecuteContext(ctx); err != nil {
		var failed exitStatus
		if errors.As(err, &failed) {
			os.Exit(int(failed))
		}
		var apiErr *apiError
		if cli.jsonOutput && errors.As(err, &apiErr) {
			detail := map[string]string{
				"code": apiErr.Detail.Code, "message": apiErr.Detail.Message, "operation_id": apiErr.Detail.OperationID,
				"effect": apiErr.Detail.Effect, "retry": apiErr.Detail.Retry,
			}
			if apiErr.Detail.Capability != "" {
				detail["capability"] = apiErr.Detail.Capability
			}
			if apiErr.Detail.ReasonCode != "" {
				detail["reason_code"] = apiErr.Detail.ReasonCode
			}
			_ = json.NewEncoder(root.ErrOrStderr()).Encode(map[string]any{"status": apiErr.Status, "error": detail})
		} else {
			fmt.Fprintln(root.ErrOrStderr(), err)
		}
		os.Exit(1)
	}
}

func rootCommand() (*cobra.Command, *client) {
	api := &client{}
	origin := os.Getenv("SANDBOX_URL")
	if origin == "" {
		origin = "https://127.0.0.1:8443"
	}
	root := &cobra.Command{Use: "sandboxctl", Short: "Manage Sandbox Platform workspaces", Version: buildinfo.Version, SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&api.origin, "url", origin, "Platform URL (SANDBOX_URL)")
	root.PersistentFlags().StringVar(&api.token, "token", os.Getenv("SANDBOX_TOKEN"), "Platform access token (prefer SANDBOX_TOKEN)")
	root.PersistentFlags().StringVar(&api.caFile, "ca-file", os.Getenv("SANDBOX_CA_FILE"), "Additional trusted server CA PEM file")
	root.PersistentFlags().BoolVar(&api.jsonOutput, "json", false, "Write compact JSON where applicable")
	root.PersistentFlags().BoolVar(&api.noWait, "no-wait", false, "Print accepted operations without polling")
	root.PersistentFlags().DurationVar(&api.waitTimeout, "wait-timeout", 3*time.Minute, "Maximum time to observe an operation")
	root.PersistentFlags().StringVar(&api.epoch, "control-epoch", "", "Original control epoch for replaying a mutation")
	root.PersistentFlags().StringVar(&api.key, "idempotency-key", "", "Idempotency key for the mutation")
	root.PersistentFlags().Int64Var(&api.revision, "revision", 0, "Original resource revision for a replay or explicit update")
	root.PersistentFlags().Int64Var(&api.generation, "generation", 0, "Original machine generation for a replay")
	root.AddCommand(api.machines(), api.tasks(), api.operations(), api.snapshots(), api.volumes(), api.services(),
		api.files(), api.leases(), api.desktops(), api.credentials(), api.templates(), api.settings(), api.events(),
		api.exec(), api.execs(), api.sshConfig(), api.sshHostKey(), api.sshProxy(), api.portForward())
	profiles := api.list("runtime-profiles", "runtime-profiles", false)
	profiles.Short = "List registered runtime profiles and bundle references"
	root.AddCommand(profiles)
	root.AddCommand(api.mcpCommand())
	root.AddCommand(api.backups())
	root.AddCommand(adminCommand(), doctorCommand(), diagnosticsCommand())
	return root, api
}

func (c *client) setup() error {
	if c.http != nil {
		return nil
	}
	base, err := url.Parse(c.origin)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return errors.New("platform URL must be an HTTP or HTTPS origin")
	}
	ip := net.ParseIP(base.Hostname())
	if base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "localhost" || ip.IsLoopback())) {
		return errors.New("HTTPS is required for a non-loopback platform URL")
	}
	if c.token == "" {
		return errors.New("provide a platform token through SANDBOX_TOKEN or --token")
	}
	if c.epoch != "" && c.key == "" || c.epoch == "" && c.key != "" {
		return errors.New("provide --control-epoch and --idempotency-key together")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.caFile != "" {
		b, err := os.ReadFile(c.caFile)
		if err != nil {
			return err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return err
		}
		if !roots.AppendCertsFromPEM(b) {
			return errors.New("CA file does not contain any certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	c.base = base
	c.http = &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return nil
}

func (c *client) endpoint(path string) (*url.URL, error) {
	if err := c.setup(); err != nil {
		return nil, err
	}
	rel, err := url.Parse(path)
	if err != nil || rel.IsAbs() || rel.Host != "" || strings.HasPrefix(rel.Path, "/") {
		return nil, errors.New("expected a relative platform API path")
	}
	for _, part := range strings.Split(rel.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("invalid path segment")
		}
	}
	out := *rel
	out.Scheme, out.Host = c.base.Scheme, c.base.Host
	out.Path, out.RawPath = "/v1/"+rel.Path, "/v1/"+rel.EscapedPath()
	return &out, nil
}

func (c *client) request(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	endpoint, err := c.endpoint(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		if key == "Content-Length" && len(values) == 1 {
			req.ContentLength, err = strconv.ParseInt(values[0], 10, 64)
			if err != nil {
				return nil, err
			}
			continue
		}
		req.Header[key] = values
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var envelope struct {
			Error *apiErrorDetail `json:"error"`
		}
		err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope)
		api := &apiError{Status: resp.StatusCode}
		if err == nil && envelope.Error != nil {
			api.Detail = *envelope.Error
		}
		return nil, api
	}
	return resp, nil
}

type apiErrorDetail = struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	OperationID string `json:"operation_id"`
	Effect      string `json:"effect"`
	Retry       string `json:"retry"`
	Capability  string `json:"capability"`
	ReasonCode  string `json:"reason_code"`
}

func (c *client) get(ctx context.Context, path string, dest any) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(dest)
}

func (c *client) print(cmd *cobra.Command, v any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	if !c.jsonOutput {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(v)
}

func (c *client) show(cmd *cobra.Command, path string) error {
	var raw json.RawMessage
	if err := c.get(cmd.Context(), path, &raw); err != nil {
		return err
	}
	return c.print(cmd, raw)
}

func (c *client) intent(ctx context.Context, rev int64) (http.Header, error) {
	if err := c.setup(); err != nil {
		return nil, err
	}
	epoch, key := c.epoch, c.key
	if key == "" {
		var status struct {
			Epoch string `json:"control_epoch"`
		}
		if err := c.get(ctx, "control", &status); err != nil {
			return nil, err
		}
		var entropy [16]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, err
		}
		epoch, key = status.Epoch, hex.EncodeToString(entropy[:])
	}
	h := make(http.Header)
	h.Set("If-Control-Epoch", epoch)
	h.Set("Idempotency-Key", key)
	if c.revision > 0 {
		rev = c.revision
	}
	if rev > 0 {
		h.Set("If-Match", fmt.Sprintf("\"%d\"", rev))
	}
	return h, nil
}

func (c *client) mutation(cmd *cobra.Command, method, path string, payload any, rev int64) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers, err := c.intent(cmd.Context(), rev)
	if err != nil {
		return err
	}
	headers.Set("Content-Type", "application/json")
	return c.write(cmd, method, path, bytes.NewReader(raw), headers)
}

func (c *client) write(cmd *cobra.Command, method, path string, body io.Reader, headers http.Header) error {
	resp, err := c.request(cmd.Context(), method, path, body, headers)
	if err != nil {
		var api *apiError
		if !errors.As(err, &api) || api.Status >= 500 && api.Detail.Effect != "none" {
			c.printIntent(cmd, headers)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var raw json.RawMessage
	if err = json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.printIntent(cmd, headers)
		return err
	}
	var op operation
	if !c.noWait && json.Unmarshal(raw, &op) == nil && op.ID != "" && op.State != "" {
		if !op.terminal() {
			if !c.jsonOutput {
				fmt.Fprintf(cmd.ErrOrStderr(), "Observing operation %s\n", op.ID)
			}
			return c.wait(cmd, op.ID)
		}
		return c.operationResult(cmd, raw, op)
	}
	return c.print(cmd, raw)
}

func (c *client) printIntent(cmd *cobra.Command, h http.Header) {
	flags := ""
	if revision := h.Get("If-Match"); revision != "" {
		flags = " --revision " + strings.Trim(revision, "\"")
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Mutation intent --control-epoch %s --idempotency-key %s%s\n",
		h.Get("If-Control-Epoch"), h.Get("Idempotency-Key"), flags)
}

func (c *client) operationResult(cmd *cobra.Command, raw json.RawMessage, op operation) error {
	if err := c.print(cmd, raw); err != nil {
		return err
	}
	if op.State != "succeeded" {
		return exitStatus(1)
	}
	return nil
}

func (c *client) wait(cmd *cobra.Command, id string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), c.waitTimeout)
	defer cancel()
	path := "operations/" + url.PathEscape(id)
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	for {
		var raw json.RawMessage
		if err := c.get(ctx, path, &raw); err != nil {
			return fmt.Errorf("observe operation %s: %w", id, err)
		}
		var op operation
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if op.terminal() {
			return c.operationResult(cmd, raw, op)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("observe operation %s: %w", id, ctx.Err())
		case <-ticker.C:
		}
	}
}

func res(kind, id string) string {
	return kind + "/" + url.PathEscape(id)
}

func (c *client) machine(ctx context.Context, id string) (machine, error) {
	var m machine
	if c.revision > 0 && c.generation > 0 {
		return machine{ID: id, Revision: c.revision, Generation: c.generation}, nil
	}
	err := c.get(ctx, res("machines", id), &m)
	if c.generation > 0 {
		m.Generation = c.generation
	}
	return m, err
}
