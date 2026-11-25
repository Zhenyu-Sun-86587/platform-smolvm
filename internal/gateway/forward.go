// Package gateway forwards authorized connections to validated loopback publications.
package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const ForwardingHeader = "X-Sandbox-Forwarding"

type Backend struct {
	Port       uint16
	Protocol   string
	ServerName string
}

// Dial is called with a backend resolved from the current binding and port table.
// Recheck revalidates binding identity and service revision at the dial boundary.
func (b Backend) Dial(ctx context.Context, recheck func(context.Context) error) (net.Conn, error) {
	if b.Port == 0 {
		return nil, errors.New("service has no loopback port")
	}
	if err := recheck(ctx); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 8 * time.Second}
	return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(b.Port))))
}
func clean(h http.Header) {
	for k := range h {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-sandbox-") || strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "proxy-authorization" || lower == "x-real-ip" {
			h.Del(k)
		}
	}
}
func reservedCookie(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "__host-sandbox-")
}
func StripRequest(r *http.Request) {
	clean(r.Header)
	r.Header.Del("Authorization")
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if !reservedCookie(c.Name) {
			r.AddCookie(c)
		}
	}
}

type trackedConn struct {
	net.Conn
	started *atomic.Bool
}

func (c trackedConn) Write(b []byte) (int, error) { c.started.Store(true); return c.Conn.Write(b) }
func (c trackedConn) CloseWrite() error {
	if t, ok := c.Conn.(*net.TCPConn); ok {
		return t.CloseWrite()
	}
	return nil
}

// HTTP uses a fresh transport for each request. Even if Transport asks for a
// second connection the single-dial guard rejects it before writing any bytes.
func (b Backend) HTTP(w http.ResponseWriter, r *http.Request, check func(context.Context) error) {
	var dialed, started atomic.Bool
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Minute, TLSHandshakeTimeout: 8 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: b.ServerName}}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("backend request may not be replayed")
		}
		conn, err := b.Dial(ctx, check)
		if err != nil {
			return nil, err
		}
		return trackedConn{conn, &started}, nil
	}
	defer transport.CloseIdleConnections()
	proxy := httputil.ReverseProxy{Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = "http"
			if b.Protocol == "https" {
				p.Out.URL.Scheme = "https"
			}
			p.Out.URL.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(b.Port)))
			p.Out.Host = p.In.Host
			p.Out.GetBody = nil
			StripRequest(p.Out)
			p.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			clean(resp.Header)
			clean(resp.Trailer)
			cookies := resp.Header.Values("Set-Cookie")
			resp.Header.Del("Set-Cookie")
			for _, v := range cookies {
				name, _, ok := strings.Cut(v, "=")
				if ok && !reservedCookie(strings.TrimSpace(name)) && !strings.Contains(strings.ToLower(v), "; domain=") && !strings.Contains(strings.ToLower(v), ";domain=") {
					resp.Header.Add("Set-Cookie", v)
				}
			}
			// Guest trailers could contain platform headers announced after the initial response.
			resp.Trailer = nil
			resp.Header.Del("Trailer")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			stage := "not_started"
			if started.Load() {
				stage = "may_have_started"
			}
			w.Header().Set(ForwardingHeader, stage)
			http.Error(w, "Service backend unavailable", 502)
		}}
	proxy.ServeHTTP(w, r)
}
func (b Backend) Tunnel(ctx context.Context, ws *websocket.Conn, check func(context.Context) error) error {
	conn, err := b.Dial(ctx, check)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws.SetReadLimit(1 << 20)
	if err = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`)); err != nil {
		return err
	}
	result := make(chan error, 2)
	clientHalf, backendHalf := make(chan struct{}), make(chan struct{})
	go func() {
		half := false
		for {
			kind, msg, e := ws.Read(ctx)
			if e != nil {
				result <- e
				return
			}
			if kind == websocket.MessageBinary && !half {
				if _, e = conn.Write(msg); e != nil {
					result <- e
					return
				}
				continue
			}
			var control struct {
				Type string `json:"type"`
			}
			if kind != websocket.MessageText || json.Unmarshal(msg, &control) != nil || control.Type != "close_write" || half {
				result <- errors.New("invalid tunnel frame")
				return
			}
			half = true
			close(clientHalf)
			e = conn.(*net.TCPConn).CloseWrite()
			if e != nil {
				result <- e
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, e := conn.Read(buf)
			if n > 0 {
				if we := ws.Write(ctx, websocket.MessageBinary, buf[:n]); we != nil {
					result <- we
					return
				}
			}
			if e != nil {
				if e == io.EOF {
					e = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"close_write"}`))
					if e == nil {
						close(backendHalf)
						return
					}
				}
				result <- e
				return
			}
		}
	}()
	select {
	case err := <-result:
		return err
	case <-backendHalf:
		select {
		case <-clientHalf:
			return nil
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b Backend) URL() string {
	scheme := "http"
	if b.Protocol == "https" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(b.Port)))
}
