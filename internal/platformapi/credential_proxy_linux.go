package platformapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

var errProxyLimit = errors.New("credential proxy byte budget exceeded")

func proxyFailure() error {
	return &rt.Error{Code: "UPSTREAM_UNAVAILABLE", Status: http.StatusBadGateway, Message: "credential upstream unavailable"}
}

func hostCredential(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, proxyFailure()
	}
	file := os.NewFile(uintptr(fd), "credential")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return nil, proxyFailure()
	}
	secret, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(secret) > 4096 {
		return nil, proxyFailure()
	}
	secret = bytes.TrimSpace(secret)
	if len(secret) == 0 || len(bytes.Trim(secret, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~+/=")) != 0 {
		return nil, proxyFailure()
	}
	return secret, nil
}

func usableProxyAddress(addr netip.Addr, allowPrivate bool) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false
	}
	return allowPrivate || (addr.IsGlobalUnicast() && !addr.IsPrivate() && !addr.IsLoopback() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(addr))
}

func proxyTransport(policy *config.CredentialProxy, origin *url.URL) (*http.Transport, error) {
	var roots *x509.CertPool
	if policy.CAFile != "" {
		roots, _ = x509.SystemCertPool()
		if roots == nil {
			roots = x509.NewCertPool()
		}
		pem, err := os.ReadFile(policy.CAFile)
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			return nil, proxyFailure()
		}
	}
	return &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: origin.Hostname()},
		TLSHandshakeTimeout: 10 * time.Second, DisableCompression: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || !strings.EqualFold(host, origin.Hostname()) || port != config.ProxyPort(origin) {
				return nil, proxyFailure()
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, proxyFailure()
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			for _, addr := range addresses {
				if usableProxyAddress(addr, policy.AllowPrivate) {
					if client, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port)); err == nil {
						return client, nil
					}
				}
			}
			return nil, proxyFailure()
		},
	}, nil
}

func (s *Server) serveCredentialProxy(w http.ResponseWriter, r *http.Request, peer auth.AttestedGuestContext,
	secret string, verify guestVerifier) error {
	policyID := r.PathValue("proxy")
	caller, err := s.Auth.AuthenticateGuest(r.Context(), peer, secret, auth.GuestProxy, policyID)
	if err != nil {
		return err
	}
	policy := s.Config.CredentialProxy(policyID)
	path := "/" + r.PathValue("route")
	if policy == nil || !policy.Allows(r.Method, path) || r.URL.EscapedPath() != "/v1/credential-proxies/"+policyID+path {
		return auth.ErrForbidden
	}
	ctx, finish := context.WithTimeout(r.Context(), time.Duration(policy.TimeoutSeconds)*time.Second)
	defer finish()
	_, release, err := s.Auth.BeginGuestCall(ctx, peer, secret, auth.GuestProxy, policyID,
		time.Duration(policy.TimeoutSeconds+5)*time.Second, policy.RequestsPerMinute,
		func(_ *sql.Tx, current auth.GuestPrincipal) error {
			if current != caller {
				return auth.ErrForbidden
			}
			return nil
		})
	if err != nil {
		return err
	}
	defer release()
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, policy.RequestBytes))
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			return &rt.Error{Code: "RESOURCE_LIMIT", Status: http.StatusRequestEntityTooLarge, Message: errProxyLimit.Error()}
		}
		return registry.ErrInvalid
	}
	check := func(ctx context.Context) error {
		if _, err := verify(r.WithContext(ctx)); err != nil {
			return err
		}
		return s.Auth.CheckGuestGrant(ctx, peer, secret, caller)
	}
	if err := check(ctx); err != nil {
		return err
	}
	binding, err := s.DB.GetBinding(ctx, peer.BindingID)
	if err != nil {
		return err
	}
	stopActivity, err := s.holdGuestRelayActivity(ctx, binding, caller.GrantID, "credential_proxy")
	if err != nil {
		return err
	}
	defer stopActivity()
	if err := check(ctx); err != nil {
		return err
	}
	call, stopWatching := watchGuestAuthority(ctx, check)
	defer stopWatching(nil)
	key, err := hostCredential(policy.CredentialFile)
	if err != nil {
		return err
	}
	defer clear(key)
	origin, err := url.Parse(policy.Upstream)
	if err != nil {
		return proxyFailure()
	}
	transport, err := proxyTransport(policy, origin)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	upstream := *origin
	upstream.Path, upstream.RawQuery = path, r.URL.RawQuery
	request, err := http.NewRequestWithContext(call, r.Method, upstream.String(), bytes.NewReader(body))
	if err != nil {
		return proxyFailure()
	}
	for _, name := range []string{"Content-Type", "Accept", "Last-Event-ID"} {
		if value := r.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	request.Header.Set("Authorization", "Bearer "+string(key))
	client := &http.Client{Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) > 3 || next.URL.Scheme != "https" || !strings.EqualFold(next.URL.Hostname(), origin.Hostname()) ||
			config.ProxyPort(next.URL) != config.ProxyPort(origin) || next.URL.User != nil || next.URL.RawPath != "" ||
			!policy.Allows(next.Method, next.URL.Path) {
			return proxyFailure()
		}
		return check(next.Context())
	}}
	status, transferred := http.StatusBadGateway, int64(0)
	defer func() {
		audit, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		tx, err := s.DB.DB().BeginTx(audit, nil)
		if err == nil {
			defer tx.Rollback()
			if registry.AppendEvent(audit, tx, "auth", peer.MachineID, "", map[string]any{
				"action": auth.GuestProxy, "proxy_id": policyID, "method": r.Method, "status": status, "response_bytes": transferred,
			}, true) == nil {
				_ = tx.Commit()
			}
		}
	}()
	response, err := client.Do(request)
	if err != nil {
		if cause := context.Cause(call); cause != nil {
			return cause
		}
		return proxyFailure()
	}
	defer response.Body.Close()
	if err := check(call); err != nil {
		return err
	}
	encoding := response.Header.Get("Content-Encoding")
	if response.ContentLength > policy.ResponseBytes || (encoding != "" && encoding != "identity") {
		return proxyFailure()
	}
	status = response.StatusCode
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	switch media {
	case "application/json", "application/octet-stream", "text/event-stream", "text/plain":
		w.Header().Set("Content-Type", media)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(status)
	transferred, err = streamProxyResponse(call, w, response.Body, key, policy.ResponseBytes)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func streamProxyResponse(ctx context.Context, w http.ResponseWriter, r io.Reader, key []byte, maximum int64) (int64, error) {
	var transfer int64
	var tail []byte
	buff := make([]byte, 32<<10)
	controller := http.NewResponseController(w)
	for {
		n, stop := r.Read(buff)
		if err := ctx.Err(); err != nil {
			return transfer, err
		}
		if int64(n) > maximum-transfer {
			return transfer, errProxyLimit
		}
		transfer += int64(n)
		pending := append(tail, buff[:n]...)
		for index := bytes.Index(pending, key); index >= 0; index = bytes.Index(pending, key) {
			if _, err := w.Write(pending[:index]); err != nil {
				return transfer, err
			}
			if _, err := io.WriteString(w, "[redacted]"); err != nil {
				return transfer, err
			}
			pending = pending[index+len(key):]
		}
		carry := 0
		if stop == nil {
			for carry = min(len(key)-1, len(pending)); carry > 0; carry-- {
				if bytes.Equal(pending[len(pending)-carry:], key[:carry]) {
					break
				}
			}
		}
		if _, err := w.Write(pending[:len(pending)-carry]); err != nil {
			return transfer, err
		}
		_ = controller.Flush()
		if stop != nil {
			if stop == io.EOF {
				return transfer, nil
			}
			return transfer, stop
		}
		tail = append(tail[:0], pending[len(pending)-carry:]...)
	}
}
