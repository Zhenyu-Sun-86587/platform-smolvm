package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func TestFileUploadUnknownPreservesOriginalIntent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Idempotency-Key") != "original-key" {
			t.Errorf("unexpected upload: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"OUTCOME_UNKNOWN","effect":"unknown","retry":"verify_original_intent"}}`)
	}))
	defer server.Close()

	api := &client{origin: server.URL, token: "test"}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	err := api.write(cmd, http.MethodPut, "machines/m/files?path=/tmp/file", strings.NewReader("data"), http.Header{
		"If-Control-Epoch": {"original-epoch"}, "Idempotency-Key": {"original-key"}, "If-Match": {`"2"`},
	}, 0)
	var failure *apiError
	if !errors.As(err, &failure) || failure.Detail.Effect != "unknown" {
		t.Fatalf("upload returned %v", err)
	}
	if !strings.Contains(stderr.String(), "--control-epoch original-epoch --idempotency-key original-key --revision 2") {
		t.Fatalf("original intent absent: %s", stderr.String())
	}
}

func TestMutationDoesNotReplayAfterAReusedConnectionLosesTheResponse(t *testing.T) {
	for _, body := range []string{"", "canary"} {
		t.Run("body length "+strconv.Itoa(len(body)), func(t *testing.T) {
			var calls atomic.Int32
			var primedAddress string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/control" {
					primedAddress = r.RemoteAddr
					_, _ = io.WriteString(w, `{"control_epoch":"test"}`)
					return
				}
				if r.Method != http.MethodPut || r.Header.Get("Idempotency-Key") != "original" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				payload, err := io.ReadAll(r.Body)
				if err != nil || string(payload) != body {
					t.Errorf("unexpected mutation body: %q %v", payload, err)
				}
				if calls.Add(1) == 1 && r.RemoteAddr != primedAddress {
					t.Errorf("mutation used %s, wanted the reused connection %s", r.RemoteAddr, primedAddress)
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}))
			defer server.Close()
			api := &client{origin: server.URL, token: "fixture", jsonOutput: true}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			var control any
			if err := api.get(cmd.Context(), "control", &control); err != nil {
				t.Fatal(err)
			}
			err := api.write(cmd, http.MethodPut, "machines/m/files?path=/tmp/file", strings.NewReader(body), http.Header{
				"If-Control-Epoch": {"test"}, "Idempotency-Key": {"original"},
			}, 0)
			var failure *apiError
			if !errors.As(err, &failure) || failure.Detail.Effect != "unknown" ||
				failure.Detail.Intent == nil || failure.Detail.Intent.IdempotencyKey != "original" || calls.Load() != 1 {
				t.Fatalf("mutation deliveries=%d error=%v", calls.Load(), err)
			}
		})
	}
}
