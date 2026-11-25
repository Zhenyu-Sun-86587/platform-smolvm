package gateway

import (
	"bufio"
	"context"
	"errors"
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

func TestPrivateBackendStreamClosesAfterAdmissionIsRevoked(t *testing.T) {
	closed := make(chan struct{})
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		_, _ = io.WriteString(w, "ready\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer guest.Close()
	var revoked atomic.Bool
	var dials atomic.Int32
	backend := Backend{Port: 34567, Protocol: "http", DialPrivate: func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", guest.Listener.Addr().String())
	}}
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.HTTP(w, r, func(context.Context) error {
			if revoked.Load() {
				return errors.New("test credential was revoked")
			}
			return nil
		})
	}))
	defer edge.Close()
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Get(edge.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal(line, err)
	}
	revoked.Store(true)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("guest service stream remained live after revocation")
	}
	if calls := dials.Load(); calls != 1 {
		t.Fatalf("private TCP dials = %d, want 1", calls)
	}
}
