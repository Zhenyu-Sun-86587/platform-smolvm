// Package smolvm adapts the public smolvm 1.37.1 API over a Unix socket.
package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"sandbox-platform/internal/runtime"
)

const (
	machinesPath  = "/api/v1/machines"
	responseLimit = 32 << 20
	errorLimit    = 64 << 10
)

type Client struct {
	http          *http.Client
	localImageDir string
	statusSocket  string
}

var _ runtime.Adapter = (*Client)(nil)

type Option func(*Client) error

// WithLocalImageDir enables administrator-registered local template archives.
func WithLocalImageDir(dir string) Option {
	return func(client *Client) error {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
			return invalid("local image directory must be an absolute, canonical path")
		}
		client.localImageDir = dir
		return nil
	}
}

// WithStatusSocket selects the read-only helper used to verify restart policy.
func WithStatusSocket(socket string) Option {
	return func(client *Client) error {
		client.statusSocket = socket
		return nil
	}
}

func New(socket string, options ...Option) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 107 || strings.ContainsRune(socket, 0) {
		return nil, invalid("runtime socket must be an absolute, canonical Unix socket path")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, option := range options {
		if err := option(client); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func invalid(message string) error {
	return &runtime.Error{Code: "INVALID_ARGUMENT", Message: message}
}

func makeURL(path string, query url.Values) string {
	return "http://smolvm" + path + encodedQuery(query)
}

func encodedQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}

func (client *Client) request(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader, length int64) (*http.Response, error) {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		connected.Store(true)
	}})
	req, err := http.NewRequestWithContext(ctx, method, makeURL(path, query), body)
	if err != nil {
		return nil, invalid("invalid runtime request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if body != nil && length >= 0 {
		req.ContentLength = length
		if length == 0 {
			req.Body = http.NoBody
		}
	}
	mutates := method != http.MethodGet
	if mutates {
		req.GetBody = nil
	}
	response, err := client.http.Do(req)
	if err != nil {
		return nil, &runtime.Error{Code: "RUNTIME_UNAVAILABLE", Message: err.Error(), Unknown: mutates && connected.Load(), Cause: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, statusError(response, mutates)
	}
	return response, nil
}

func statusError(response *http.Response, mutates bool) error {
	body, readErr := io.ReadAll(io.LimitReader(response.Body, errorLimit))
	var native struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &native)
	status := response.StatusCode
	code := "RUNTIME_ERROR"
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		code = "INVALID_ARGUMENT"
	case http.StatusNotFound:
		code = "NOT_FOUND"
	case http.StatusConflict:
		code = "CONFLICT"
		if native.Code == "PORT_IN_USE" {
			code = native.Code
		}
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = "RUNTIME_UNAVAILABLE"
	case http.StatusNotImplemented:
		code = "NOT_SUPPORTED"
	case http.StatusUnauthorized, http.StatusForbidden:
		code = "FORBIDDEN"
	}
	message := native.Error
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = response.Status
	}
	return &runtime.Error{
		Code: code, NativeCode: native.Code, Message: message, Status: status, Body: string(body), Cause: readErr,
		Unknown: mutates && (status == http.StatusRequestTimeout || status >= 500 || (status >= 300 && status < 400)),
	}
}

func protocolError(message string, mutates bool, cause error) error {
	return &runtime.Error{Code: "RUNTIME_PROTOCOL_ERROR", Message: message, Unknown: mutates, Cause: cause}
}

func (client *Client) json(ctx context.Context, method, path string, query url.Values, input, output any) error {
	var body io.Reader
	contentType := ""
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return invalid(fmt.Sprintf("invalid runtime request body: %v", err))
		}
		body = bytes.NewReader(data)
		contentType = "application/json"
	}
	response, err := client.request(ctx, method, path, query, contentType, body, -1)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		return nil
	}
	return decode(response.Body, output, method != http.MethodGet)
}

func decode(body io.Reader, output any, mutates bool) error {
	data, err := io.ReadAll(io.LimitReader(body, responseLimit+1))
	if err != nil {
		return protocolError("reading runtime response: "+err.Error(), mutates, err)
	}
	if len(data) > responseLimit {
		return protocolError("runtime JSON response exceeds the size limit", mutates, nil)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return protocolError("decoding runtime response: "+err.Error(), mutates, err)
	}
	return nil
}

func (client *Client) Health(ctx context.Context) (runtime.Health, error) {
	var result runtime.Health
	err := client.json(ctx, http.MethodGet, "/health", nil, nil, &result)
	return result, err
}

func (client *Client) Ready(ctx context.Context) error {
	health, err := client.Health(ctx)
	if err != nil {
		return err
	}
	if health.Version != runtime.Version {
		return &runtime.Error{Code: "RUNTIME_VERSION_MISMATCH", Message: fmt.Sprintf("runtime version %q, expected %q", health.Version, runtime.Version)}
	}
	return client.json(ctx, http.MethodGet, "/readyz", nil, nil, nil)
}
