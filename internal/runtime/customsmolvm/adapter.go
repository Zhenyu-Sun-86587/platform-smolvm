package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

// Route pins a particular Serve installation and its installed bundle.
type Route struct {
	InstanceID string `yaml:"instance_id"`
	Bundle     string `yaml:"bundle_digest"`
	Reader     string `yaml:"state_reader_id"`
	Socket     string
	Manifest   string
}

func (r Route) Key() rt.BundleRoute {
	return rt.BundleRoute{InstanceID: r.InstanceID, BundleID: r.Bundle}
}

func (r Route) Validate() error {
	if !identifier.MatchString(r.InstanceID) {
		return fmt.Errorf("%w: invalid Serve runtime instance ID", rt.ErrInvalid)
	}
	return validateEndpoint(r.Socket, r.Bundle, r.Reader)
}

type Adapter struct {
	unimplemented
	route Route
}

var _ rt.Adapter = (*Adapter)(nil)

func New(route Route) (*Adapter, error) {
	if err := route.Validate(); err != nil {
		return nil, err
	}
	return &Adapter{route: route}, nil
}

func (a *Adapter) connect(ctx context.Context) (*http.Client, Handshake, error) {
	endpoint := dialEndpoint(a.route.Socket)
	h, err := probe(ctx, endpoint, a.route.Bundle, a.route.Reader)
	if err == nil && h.RuntimeInstanceID != a.route.InstanceID {
		err = fmt.Errorf("%w: Serve runtime instance differs from the registered instance", rt.ErrUnsupported)
	}
	if err != nil {
		endpoint.CloseIdleConnections()
		return nil, Handshake{}, err
	}
	return endpoint, h, nil
}

func (a *Adapter) Health(ctx context.Context) (rt.Health, error) {
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return rt.Health{}, err
	}
	defer endpoint.CloseIdleConnections()
	var health rt.Health
	err = get(ctx, endpoint, "/health", &health)
	return health, err
}

func (a *Adapter) Ready(ctx context.Context) error {
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer endpoint.CloseIdleConnections()
	return get(ctx, endpoint, "/readyz", nil)
}

func (a *Adapter) Observe(ctx context.Context, name string) (rt.Observation, error) {
	if err := smolvm.ValidateName(name); err != nil {
		return rt.Observation{}, err
	}
	endpoint, handshake, err := a.connect(ctx)
	if err != nil {
		return rt.Observation{}, err
	}
	defer endpoint.CloseIdleConnections()
	var raw json.RawMessage
	if err := get(ctx, endpoint, "/api/v1/machines/"+name, &raw); err != nil {
		return rt.Observation{Name: name, State: rt.StateUnknown, ObservedAt: time.Now().UTC()}, err
	}
	observation, err := smolvm.DecodeObservation(raw, name)
	observation.Capabilities = handshake.Capabilities
	return observation, err
}

func (a *Adapter) QueryOperation(ctx context.Context, id string) (OperationReceipt, error) {
	if !identifier.MatchString(id) {
		return OperationReceipt{}, fmt.Errorf("%w: invalid operation ID", rt.ErrInvalid)
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer endpoint.CloseIdleConnections()
	return queryOperation(ctx, endpoint, a.route.Bundle, id)
}
