package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A backend applies a request and drops the connection before responding.
// The edge must never create a second backend connection to retry it.
func TestDroppedPOSTIsForwardedOnce(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				defer req.Body.Close()
				_, _ = io.Copy(io.Discard, req.Body)
				calls.Add(1)
				if req.Header.Get("Authorization") != "" || strings.Contains(req.Header.Get("Cookie"), "sandbox-preview") {
					t.Error("platform credential forwarded to guest")
				}
			}()
		}
	}()
	request := httptest.NewRequest("POST", "http://svc.example.test/count", strings.NewReader("increment"))
	request.Header.Set("Idempotency-Key", "counter-key")
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Cookie", "__Host-sandbox-preview=private")
	recorder := httptest.NewRecorder()
	backend := Backend{Port: uint16(l.Addr().(*net.TCPAddr).Port), Protocol: "http"}
	backend.HTTP(recorder, request, func(context.Context) error { return nil })
	if recorder.Code != 502 || recorder.Header().Get(ForwardingHeader) != "may_have_started" {
		t.Fatalf("status=%d header=%v", recorder.Code, recorder.Header())
	}
	time.Sleep(30 * time.Millisecond)
	l.Close()
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("counter=%d want 1", n)
	}
}
