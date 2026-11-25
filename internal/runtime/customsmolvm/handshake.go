// Package customsmolvm negotiates the versioned deep Serve API over a local
// operator-selected socket. Bundle selection is independent of smolvm semver.
package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const capabilitiesPath = "/api/v1/runtime-ext/v1/capabilities"

type Handshake struct {
	Schema           string                   `json:"schema"`
	RouteID          string                   `json:"route_id"`
	BundleDigest     string                   `json:"bundle_digest"`
	StateReaderID    string                   `json:"state_reader_id"`
	Protocols        map[string]int           `json:"protocols"`
	RequiredFeatures []string                 `json:"required_features"`
	Capabilities     map[string]rt.Capability `json:"capabilities"`
}

var bundleID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Probe(ctx context.Context, socket, expectedBundle, expectedReader string) (Handshake, error) {
	var h Handshake
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 107 ||
		!bundleID.MatchString(expectedBundle) || expectedBundle == "sha256:"+strings.Repeat("0", 64) || strings.TrimSpace(expectedReader) == "" {
		return h, fmt.Errorf("%w: expected canonical socket, bundle digest and reader ID", rt.ErrInvalid)
	}
	d := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://smolvm"+capabilitiesPath, nil)
	if err != nil {
		return h, err
	}
	res, err := client.Do(req)
	if err != nil {
		return h, fmt.Errorf("%w: %w", rt.ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return h, fmt.Errorf("%w: Serve has no runtime-ext/v1", rt.ErrUnsupported)
	}
	if res.StatusCode != http.StatusOK {
		return h, fmt.Errorf("%w: Serve returned %s", rt.ErrUnavailable, res.Status)
	}
	const limit = 1 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return h, err
	}
	if len(b) > limit {
		return h, fmt.Errorf("%w: handshake exceeds size limit", rt.ErrInvalid)
	}
	if err = json.Unmarshal(b, &h); err != nil {
		return Handshake{}, fmt.Errorf("%w: invalid handshake: %v", rt.ErrInvalid, err)
	}
	if h.Schema != "runtime-ext/v1" || h.RouteID != "deep" || h.BundleDigest != expectedBundle || h.StateReaderID != expectedReader ||
		h.RequiredFeatures == nil || len(h.RequiredFeatures) != 0 || h.Capabilities == nil {
		return Handshake{}, fmt.Errorf("%w: incompatible runtime schema, bundle, state reader or required feature", rt.ErrUnsupported)
	}
	for _, name := range []string{"RuntimeBundle", "WorkerResource", "StatePoint", "CaptureControl", "GuestIdentity", "NetworkAttachment", "BlockAttachment"} {
		if h.Protocols[name] != 1 {
			return Handshake{}, fmt.Errorf("%w: incompatible %s protocol", rt.ErrUnsupported, name)
		}
	}
	for key, c := range h.Capabilities {
		if (c.Status != "supported" && c.Status != "unsupported" && c.Status != "unknown") || c.Supported != (c.Status == "supported") ||
			(c.Supported && (c.VerifiedProfile == "" || c.ContractVersion != 1)) {
			return Handshake{}, fmt.Errorf("%w: invalid capability %q", rt.ErrUnsupported, key)
		}
	}
	return h, nil
}
