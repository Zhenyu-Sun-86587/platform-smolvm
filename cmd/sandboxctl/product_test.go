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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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

func TestOperationPartialCompatibilityAndStrictResult(t *testing.T) {
	const receipt = `{"operation_id":"op_fork","state":"succeeded","effect":"partial","children":[{"ordinal":0,"state":"ready"},{"ordinal":1,"state":"quarantined"}]}`
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/op_fork" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = io.WriteString(w, receipt)
	})
	for _, strict := range []bool{false, true} {
		args := []string{"--json", "operations", "wait", "op_fork"}
		expected := 0
		if strict {
			args, expected = append(args, "--strict-result"), 1
		}
		out, stderr, code := runCLI(t, handler, args, nil)
		if code != expected || stderr != "" || strings.TrimSpace(out) != receipt {
			t.Fatalf("strict=%v exit=%d stdout=%q stderr=%q", strict, code, out, stderr)
		}
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
	var response struct{ Error apiErrorDetail }
	if err := json.Unmarshal([]byte(stderr), &response); err != nil || code != 124 || out != "" ||
		response.Error.OperationID != "op_original" || response.Error.Code != "OPERATION_WAIT_TIMEOUT" ||
		response.Error.Retry != "poll_operation" || response.Error.Effect != "unknown" {
		t.Fatalf("exit=%d stdout=%q stderr=%q, %v", code, out, stderr, err)
	}
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
