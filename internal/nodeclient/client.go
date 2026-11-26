package nodeclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"sandbox-platform/internal/nodeproto"
)

type Config struct {
	Endpoint    string
	Authority   nodeproto.Authority
	TLS         *tls.Config
	NodeSPKIPin string
	Authorize   func(context.Context) error
}

type Client struct {
	endpoint  string
	authority nodeproto.Authority
	control   *http.Client
	stream    *http.Client
	authorize func(context.Context) error

	mu        sync.RWMutex
	routeInfo map[nodeproto.Route]nodeproto.RouteInfo
}

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("Node endpoint must be an HTTPS origin")
	}
	if !config.Authority.Valid() || !nodeproto.ValidDigest(config.NodeSPKIPin) || config.TLS == nil ||
		config.TLS.InsecureSkipVerify || config.TLS.RootCAs == nil ||
		(len(config.TLS.Certificates) == 0 && config.TLS.GetClientCertificate == nil) {
		return nil, errors.New("Node client requires pinned authority, peer roots and a controller certificate")
	}
	tlsConfig := config.TLS.Clone()
	tlsConfig.MinVersion = tls.VersionTLS13
	tlsConfig.NextProtos = []string{"http/1.1"}
	verify := tlsConfig.VerifyConnection
	tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
			return errors.New("Node certificate must be verified")
		}
		if err := nodeproto.VerifyPeer(cs.PeerCertificates[0], config.NodeSPKIPin, nodeproto.WorkerURI(config.Authority)); err != nil {
			return err
		}
		if verify != nil {
			return verify(cs)
		}
		return nil
	}
	newHTTPClient := func(limit int) *http.Client {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext, TLSClientConfig: tlsConfig,
				MaxConnsPerHost: limit, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 135 * time.Second,
				MaxResponseHeaderBytes: 32 << 10, DisableCompression: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	u.Path = ""
	return &Client{endpoint: u.String(), authority: config.Authority, control: newHTTPClient(nodeproto.ControlSlots + nodeproto.GuestSlots + nodeproto.StorageSlots),
		stream: newHTTPClient(nodeproto.StreamSlots), authorize: config.Authorize, routeInfo: make(map[nodeproto.Route]nodeproto.RouteInfo)}, nil
}

func (c *Client) authorizeRequest(ctx context.Context) error {
	if c.authorize != nil {
		return c.authorize(ctx)
	}
	return nil
}

func (c *Client) CloseIdleConnections() {
	c.control.CloseIdleConnections()
	c.stream.CloseIdleConnections()
}

func (c *Client) Handshake(ctx context.Context) (nodeproto.Handshake, error) {
	out, err := request[nodeproto.Empty, nodeproto.Handshake](ctx, c, nodeproto.Route{}, nodeproto.HandshakePath, nodeproto.Empty{})
	if err != nil {
		return out, err
	}
	if out.Protocol != nodeproto.Version || out.Authority != c.authority || !nodeproto.ValidID(out.NodeSession) ||
		!nodeproto.ValidID(out.HostBootID) || out.Coverage != nodeproto.KnownOnly || len(out.Routes) == 0 {
		return nodeproto.Handshake{}, &nodeproto.Failure{Code: nodeproto.CodeAuthority}
	}
	routes := make(map[nodeproto.Route]nodeproto.RouteInfo, len(out.Routes))
	for _, route := range out.Routes {
		if _, duplicate := routes[route.Route]; duplicate || !route.Valid() {
			return nodeproto.Handshake{}, &nodeproto.Failure{Code: nodeproto.CodeAuthority}
		}
		routes[route.Route] = route
	}
	c.mu.Lock()
	c.routeInfo = routes
	c.mu.Unlock()
	return out, nil
}

func (c *Client) Gate(ctx context.Context) (nodeproto.Gate, error) {
	return request[nodeproto.Empty, nodeproto.Gate](ctx, c, nodeproto.Route{}, nodeproto.GatePath, nodeproto.Empty{})
}

func (c *Client) SetGate(ctx context.Context, in nodeproto.SetGate) (nodeproto.Gate, error) {
	return request[nodeproto.SetGate, nodeproto.Gate](ctx, c, nodeproto.Route{}, nodeproto.SetGatePath, in)
}

func call[In, Out any](ctx context.Context, a *Adapter, path string, in In) (Out, error) {
	return request[In, Out](ctx, a.client, a.scope, path, in)
}

func request[In, Out any](ctx context.Context, c *Client, scope nodeproto.Route, path string, in In) (out Out, err error) {
	timeout := 20 * time.Second
	switch path {
	case nodeproto.GuestPingPath, nodeproto.GuestExecPath, nodeproto.GuestUploadPath, nodeproto.VolumeApplyPath:
		timeout = 135 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if scope != (nodeproto.Route{}) || path == nodeproto.SetGatePath {
		if err := c.authorizeRequest(ctx); err != nil {
			return out, err
		}
	}
	envelope := nodeproto.Envelope[In]{Protocol: nodeproto.Version, Authority: c.authority, Route: scope, Request: in}
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > nodeproto.ControlRequestLimit {
		return out, &nodeproto.Failure{Code: nodeproto.CodeInvalid}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	// A POST never provides a replayable body or an HTTP idempotency header.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	res, err := c.control.Do(req)
	if err != nil {
		return out, transportError(path)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, nodeproto.ControlReplyLimit+1))
	if err != nil || len(data) > nodeproto.ControlReplyLimit {
		return out, transportError(path)
	}
	if res.StatusCode != http.StatusOK {
		var failure nodeproto.Failure
		if json.Unmarshal(data, &failure) != nil || failure.Code == "" {
			return out, transportError(path)
		}
		return out, &failure
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, transportError(path)
	}
	return out, nil
}

func transportError(path string) error {
	switch path {
	case nodeproto.SubmitPowerPath, nodeproto.SubmitLaunchPath, nodeproto.SubmitForkPath, nodeproto.SubmitStatePointPath,
		nodeproto.RetryForkPath, nodeproto.CancelForkPath, nodeproto.GuestExecPath, nodeproto.GuestUploadPath, nodeproto.VolumeApplyPath:
		return &nodeproto.Failure{Code: nodeproto.CodeUnknown}
	default:
		return &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
}
