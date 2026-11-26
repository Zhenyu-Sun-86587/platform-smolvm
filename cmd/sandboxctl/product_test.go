package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"
)

func runCLI(t *testing.T, handler http.Handler, args []string, stdin io.Reader) (string, string, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	root, client := rootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if stdin != nil {
		root.SetIn(stdin)
	}
	root.SetArgs(append([]string{"--url", server.URL, "--token", "fixture"}, args...))
	code := 0
	if err := root.ExecuteContext(context.Background()); err != nil {
		code = reportError(&stderr, client.jsonOutput, err)
	}
	return stdout.String(), stderr.String(), code
}

type cliExample struct {
	Exit       int             `json:"exit"`
	StrictExit int             `json:"strict_exit"`
	Stdout     json.RawMessage `json:"stdout"`
	Stderr     json.RawMessage `json:"stderr"`
}

func cliExamples(t *testing.T) map[string]cliExample {
	t.Helper()
	raw, err := os.ReadFile("../../api/fixtures/polish-cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]cliExample
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	delete(all, "p0_revision")
	cases = make(map[string]cliExample, len(all))
	for name, value := range all {
		var example cliExample
		if err := json.Unmarshal(value, &example); err != nil {
			t.Fatal(err)
		}
		cases[name] = example
	}
	return cases
}

func equalJSON(t *testing.T, got string, want json.RawMessage) {
	t.Helper()
	var left, right any
	if err := json.Unmarshal([]byte(got), &left); err != nil {
		t.Fatalf("result is not a single JSON object: %q, %v", got, err)
	}
	if err := json.Unmarshal(want, &right); err != nil || !reflect.DeepEqual(left, right) {
		t.Fatalf("JSON %s differs from fixture %s (%v)", got, want, err)
	}
}

func TestOperationPartialCompatibilityAndStrictResult(t *testing.T) {
	example := cliExamples(t)["partial_fork"]
	receipt := string(example.Stdout)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/op_fork" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = io.WriteString(w, receipt)
	})
	for _, strict := range []bool{false, true} {
		args := []string{"--json", "operations", "wait", "op_fork"}
		expected := example.Exit
		if strict {
			args, expected = append(args, "--strict-result"), example.StrictExit
		}
		out, stderr, code := runCLI(t, handler, args, nil)
		if code != expected || stderr != "" {
			t.Fatalf("strict=%v exit=%d stdout=%q stderr=%q", strict, code, out, stderr)
		}
		equalJSON(t, out, example.Stdout)
	}
}

func TestOperationTimeoutKeepsAcceptedID(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/op_original" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = io.WriteString(w, `{"operation_id":"op_original","state":"observing","effect":"none"}`)
	})
	out, stderr, code := runCLI(t, handler, []string{"--json", "--wait-timeout", "100ms", "operations", "wait", "op_original"}, nil)
	example := cliExamples(t)["operation_wait_timeout"]
	if code != example.Exit || out != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	equalJSON(t, stderr, example.Stderr)
}

func TestDifferentMachineIDsWithExactNameAreAmbiguous(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"machine_id":"m_first","name":"my VM"}],"next_cursor":"first"}`)
		case "first":
			_, _ = io.WriteString(w, `{"items":[{"machine_id":"m_other","name":"my VM"}]}`)
		default:
			t.Error("unexpected machine cursor")
		}
	})
	out, stderr, code := runCLI(t, handler, []string{"--json", "machines", "rename", "name:my VM", "new"}, nil)
	if code != 1 || out != "" || !json.Valid([]byte(stderr)) || !strings.Contains(stderr, "ambiguous") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

func TestGuardedCommandsUseExpectedPowerAndVMOnlyModes(t *testing.T) {
	tests := []struct {
		args []string
		path string
		body string
	}{
		{[]string{"machines", "wake-full", "m_target", "--from", "standby"}, "/v1/machines/m_target/wake", `{"expected_generation":7,"expected_power_state":"standby"}`},
		{[]string{"machines", "wake-full", "m_target", "--from", "hibernated"}, "/v1/machines/m_target/start", `{"expected_generation":7,"expected_power_state":"hibernated"}`},
		{[]string{"machines", "cold-start", "m_target"}, "/v1/machines/m_target/start", `{"expected_generation":7,"expected_power_state":"stopped"}`},
		{[]string{"machines", "live-fork", "m_target", "new"}, "/v1/machines/m_target/forks", `{"child_name":"new","expected_generation":7,"require_independent":true}`},
		{[]string{"machines", "fork", "m_target", "new"}, "/v1/machines/m_target/forks", `{"child_name":"new","expected_generation":7}`},
		{[]string{"snapshots", "capture-vm", "m_target"}, "/v1/machines/m_target/snapshots", `{"capture_mode":"vm","expected_generation":7,"label":"","purpose":"recovery"}`},
		{[]string{"snapshots", "capture", "m_target"}, "/v1/machines/m_target/snapshots", `{"expected_generation":7,"label":"","purpose":"clone"}`},
		{[]string{"snapshots", "restore-vm", "snap_old", "new", "--volume-restore-policy", "exact"}, "/v1/restores", `{"name":"new","require_capture_mode":"vm","snapshot_id":"snap_old","volume_restore_policy":"exact"}`},
		{[]string{"snapshots", "rollback-vm", "m_target", "snap_old"}, "/v1/machines/m_target/rollback", `{"expected_generation":7,"require_capture_mode":"vm","snapshot_id":"snap_old","volume_restore_policy":"clone_conflicts"}`},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args[:2], " "), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tc.path || r.Header.Get("If-Match") != `"4"` {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var wanted, observed map[string]any
				_ = json.Unmarshal([]byte(tc.body), &wanted)
				if err := json.NewDecoder(r.Body).Decode(&observed); err != nil || !reflect.DeepEqual(observed, wanted) {
					t.Errorf("payload=%+v, want %+v (%v)", observed, wanted, err)
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"operation_id":"op_one","state":"queued","effect":"none"}`)
			})
			args := append([]string{"--json", "--no-wait", "--control-epoch", "original", "--idempotency-key", "same", "--revision", "4", "--generation", "7"}, tc.args...)
			out, stderr, code := runCLI(t, handler, args, nil)
			if code != 0 || stderr != "" || !strings.Contains(out, "op_one") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
			}
		})
	}
}

func TestExecFollowStreamsBytesAndGuestExit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines/m_target/exec":
			if r.Method != http.MethodPost {
				t.Errorf("unexpected request method: %s", r.Method)
			}
			_, _ = io.WriteString(w, `{"exec_id":"exec_first","next":2,"end":2,"done":false,"output":[{"stream":"stdout","data":"AP8="}]}`)
		case "/v1/execs/exec_first/output":
			if r.URL.Query().Get("cursor") != "2" {
				t.Errorf("wrong cursor %s", r.URL)
			}
			_, _ = io.WriteString(w, `{"exec_id":"exec_first","next":3,"end":3,"done":true,"exit_code":17,"output":[{"stream":"stderr","data":"AA=="}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	args := []string{"--control-epoch", "e", "--idempotency-key", "k", "--revision", "1", "--generation", "2", "exec", "--follow", "m_target", "--", "/bin/true"}
	out, stderr, code := runCLI(t, handler, args, nil)
	if code != 17 || out != "\x00\xff" || stderr != "\x00" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

func TestExecTTYTransmitsInputAndGuestExit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines/m_target/exec":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["tty"] != true || body["interactive"] != true ||
				body["cols"] != float64(80) || body["rows"] != float64(24) {
				t.Errorf("unexpected terminal request: %+v %v", body, err)
			}
			_, _ = io.WriteString(w, `{"exec_id":"exec_first","next":0,"end":0,"done":false}`)
		case "/v1/execs/exec_first/attach":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := wsjson.Write(ctx, ws, terminalFrame{Type: "ready", StdinSeq: 4}); err != nil {
				t.Error(err)
				return
			}
			var data, eof terminalInput
			if err := wsjson.Read(ctx, ws, &data); err != nil || data.Type != "stdin" || data.Seq != 5 || string(data.Data) != "hello\n" {
				t.Errorf("stdin = %+v, %v", data, err)
				return
			}
			if err := wsjson.Read(ctx, ws, &eof); err != nil || eof.Type != "stdin" || eof.Seq != 6 || !eof.EOF {
				t.Errorf("EOF = %+v, %v", eof, err)
				return
			}
			if err := ws.Write(ctx, websocket.MessageBinary, []byte("echo hello\r\n")); err != nil {
				t.Error(err)
				return
			}
			exit := 7
			if err := wsjson.Write(ctx, ws, terminalFrame{Type: "exit", ExitCode: &exit}); err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	args := []string{"--control-epoch", "e", "--idempotency-key", "k", "--revision", "1", "--generation", "2", "exec", "--tty", "m_target", "--", "/bin/sh"}
	out, stderr, code := runCLI(t, handler, args, strings.NewReader("hello\n"))
	if code != 7 || out != "echo hello\r\n" || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

func TestJSONMutationUncertaintyKeepsSingleEnvelopeAndRequestID(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusAccepted} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/m_target/start" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL)
			}
			w.WriteHeader(status)
			if status == http.StatusConflict {
				_, _ = fmt.Fprint(w, `{"error":{"request_id":"req_test","code":"OUTCOME_UNKNOWN","effect":"unknown","retry":"verify_original_intent"}}`)
			} else {
				_, _ = fmt.Fprint(w, `{"operation_id":`)
			}
		})
		args := []string{"--json", "--control-epoch", "e", "--idempotency-key", "k", "--revision", "1", "--generation", "2", "machines", "cold-start", "m_target"}
		out, stderr, code := runCLI(t, handler, args, nil)
		var response struct{ Error apiErrorDetail }
		err := json.Unmarshal([]byte(stderr), &response)
		intent := response.Error.Intent
		if err != nil || code != 1 || out != "" || response.Error.Effect != "unknown" || response.Error.Retry != "verify_original_intent" ||
			intent == nil || intent.ControlEpoch != "e" || intent.IdempotencyKey != "k" || intent.Revision != 1 || intent.Generation != 2 ||
			(status == http.StatusConflict && response.Error.RequestID != "req_test") {
			t.Fatalf("HTTP %d exit=%d stdout=%q stderr=%q, %v", status, code, out, stderr, err)
		}
	}
}

func TestCLIConflictExamplesSurviveHTTPMapping(t *testing.T) {
	examples := cliExamples(t)
	for _, name := range []string{"file_upload_outcome_unknown", "capture_mode_conflict", "power_state_conflict"} {
		t.Run(name, func(t *testing.T) {
			example := examples[name]
			var expected struct {
				Status int            `json:"status"`
				Error  apiErrorDetail `json:"error"`
			}
			if err := json.Unmarshal(example.Stderr, &expected); err != nil {
				t.Fatal(err)
			}
			intent := expected.Error.Intent
			expected.Error.Intent = nil
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(expected.Status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": expected.Error})
			}))
			defer server.Close()
			client := &client{origin: server.URL, token: "fixture", jsonOutput: true}
			var stderr bytes.Buffer
			var err error
			if intent != nil {
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				cmd.SetErr(&stderr)
				err = client.write(cmd, intent.Method, intent.Path, strings.NewReader("data"), http.Header{
					"If-Control-Epoch": {intent.ControlEpoch}, "Idempotency-Key": {intent.IdempotencyKey},
					"If-Match": {fmt.Sprintf("%q", strconv.FormatInt(intent.Revision, 10))},
				}, intent.Generation)
			} else {
				_, err = client.request(context.Background(), http.MethodPost, "machines/m_guest/start", strings.NewReader("{}"), nil)
			}
			if err == nil || reportError(&stderr, true, err) != example.Exit {
				t.Fatalf("expected exit %d for %v", example.Exit, err)
			}
			equalJSON(t, stderr.String(), example.Stderr)
		})
	}
}
