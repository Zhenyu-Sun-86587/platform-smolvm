package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
