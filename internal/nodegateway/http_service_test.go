package nodegateway

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

func TestRemoteHTTPPostBodyIsForwardedOnceAfterLostResponse(t *testing.T) {
	const body = "apply-once"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ca := newTestCA(t)
	controller, pin := ca.leaf(t, nodeproto.ControllerURI(testAuthority), x509.ExtKeyUsageClientAuth)
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	var accepted atomic.Int32
	received := make(chan error, 1)
	go func() {
		conn, err := local.Accept()
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		accepted.Add(1)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r, err := http.ReadRequest(bufio.NewReader(conn))
		if err == nil {
			defer r.Body.Close()
			data, readErr := io.ReadAll(r.Body)
			if readErr != nil || string(data) != body {
				err = fmt.Errorf("guest received %q / %v", data, readErr)
			}
		}
		// Apply the HTTP request and drop the connection before any response.
		received <- err
	}()
	node := testNode(t, ca, testAuthority, testRoute, fake("worker-a"),
		testJournal(t, filepath.Join(t.TempDir(), "journal"), testAuthority), controller, pin,
		func(s *Server, mux *http.ServeMux) {
			stream(s, mux, nodeproto.ServiceStreamPath, func(in nodeproto.ServiceStream) rt.ExecutionRef { return in.Expected },
				func(ctx context.Context, _ Route, _ nodeproto.ServiceStream) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", local.Addr().String())
				})
		})
	var dials atomic.Int32
	adapter := node.client.Adapter(testRoute)
	backend := gateway.Backend{Port: 8080, Protocol: "http", DialPrivate: func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		return adapter.DialService(ctx, testRef, 8080)
	}}
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://svc.example.test/mutate", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", "guest-controlled-key")
	result := httptest.NewRecorder()
	backend.HTTP(result, r, func(context.Context) error { return nil })
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("remote guest did not receive the POST")
	}
	if result.Code != http.StatusBadGateway || result.Header().Get(gateway.ForwardingHeader) != "may_have_started" ||
		dials.Load() != 1 || accepted.Load() != 1 {
		t.Fatalf("POST replay or forwarding status differed: status=%d header=%v dials=%d accept=%d",
			result.Code, result.Header(), dials.Load(), accepted.Load())
	}
}
