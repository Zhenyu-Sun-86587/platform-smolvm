// Package servicehealth records evidence produced by service access. Reading
// observations performs no network activity and acquires no machine access.
package servicehealth

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const (
	StageConfiguration   = "configuration"
	StageWake            = "wake"
	StageGuestManagement = "guest_management"
	StageWorkerUpstream  = "worker_upstream"

	freshFor    = 2 * time.Minute
	maxServices = 4096
)

type Identity struct {
	ServiceRevision   int64
	BindingID         string
	BindingGeneration int64
	IncarnationID     string
	ExecutionEpoch    int64
}

type Observation struct {
	Status            string     `json:"status"`
	Stage             string     `json:"stage"`
	Scope             string     `json:"scope"`
	ReasonCode        string     `json:"reason_code"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	ServiceRevision   int64      `json:"service_revision"`
	BindingGeneration int64      `json:"binding_generation"`
	ExecutionEpoch    int64      `json:"execution_epoch"`
	OperationID       string     `json:"operation_id,omitempty"`
}

type sample struct {
	key         Identity
	observation Observation
}

// Tracker keeps the last evidence per service in a bounded process-local
// cache. Unobserved services, including after restart, return unknown.
type Tracker struct {
	mu     sync.RWMutex
	latest map[string]sample
}

func (t *Tracker) Record(serviceID string, key Identity, obs Observation) {
	if serviceID == "" || key.ServiceRevision <= 0 || key.BindingID == "" || !obs.Recordable() {
		return
	}
	now := time.Now().UTC()
	if obs.ObservedAt != nil {
		now = obs.ObservedAt.UTC()
	}
	obs.ObservedAt = &now
	obs = obs.identity(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.latest == nil {
		t.latest = make(map[string]sample)
	}
	if prior, exists := t.latest[serviceID]; exists {
		if older(key, prior.key) || key == prior.key && now.Before(*prior.observation.ObservedAt) {
			return
		}
	} else if len(t.latest) >= maxServices {
		var eldest string
		var earliest time.Time
		for id, value := range t.latest {
			if eldest == "" || value.observation.ObservedAt.Before(earliest) {
				eldest, earliest = id, *value.observation.ObservedAt
			}
		}
		delete(t.latest, eldest)
	}
	t.latest[serviceID] = sample{key, obs}
}

func older(key, prior Identity) bool {
	if key.ServiceRevision != prior.ServiceRevision {
		return key.ServiceRevision < prior.ServiceRevision
	}
	if key.BindingGeneration != prior.BindingGeneration {
		return key.BindingGeneration < prior.BindingGeneration
	}
	return key.ExecutionEpoch < prior.ExecutionEpoch
}

func (t *Tracker) View(serviceID string, key Identity, enabled, running bool) Observation {
	if !enabled {
		return Unavailable(StageConfiguration, "SERVICE_DISABLED").identity(key)
	}
	t.mu.RLock()
	prior, ok := t.latest[serviceID]
	t.mu.RUnlock()
	if !ok {
		return observation("unknown", StageConfiguration, "SERVICE_NOT_OBSERVED").identity(key)
	}
	obs := prior.observation
	observedAt := *obs.ObservedAt
	obs.ObservedAt = &observedAt
	switch {
	case prior.key != key:
		obs.Status = "stale"
		obs.ReasonCode = "SERVICE_IDENTITY_CHANGED"
	case !running && obs.Stage != StageWake:
		obs.Status = "stale"
		obs.ReasonCode = "EXECUTION_NOT_RUNNING"
	case time.Since(observedAt) > freshFor:
		obs.Status = "stale"
		obs.ReasonCode = "OBSERVATION_EXPIRED"
	}
	return obs
}

func (obs Observation) identity(key Identity) Observation {
	obs.ServiceRevision = key.ServiceRevision
	obs.BindingGeneration = key.BindingGeneration
	obs.ExecutionEpoch = key.ExecutionEpoch
	return obs
}

func observation(status, stage, reason string) Observation {
	scope := "configuration"
	if stage == StageWorkerUpstream || stage == StageGuestManagement {
		scope = "worker_upstream"
	}
	return Observation{Status: status, Stage: stage, Scope: scope, ReasonCode: reason}
}

func Unavailable(stage, reason string) Observation {
	return observation("unavailable", stage, reason)
}

func WakePending() Observation {
	return observation("unknown", StageWake, "WAKE_ACCESS_PENDING")
}

func (o Observation) Recordable() bool {
	switch o.ReasonCode {
	case "", "AUTHORIZATION_REQUIRED", "ACCESS_CANCELED", "SERVICE_IDENTITY_CHANGED":
		return false
	}
	return o.Status == "ready" || o.Status == "unavailable" || o.ReasonCode == "WAKE_ACCESS_PENDING"
}

func Diagnose(stage string, err error) Observation {
	if err == nil {
		switch stage {
		case StageWorkerUpstream:
			return observation("ready", stage, "UPSTREAM_REACHED")
		case StageGuestManagement:
			return observation("ready", stage, "GUEST_IDENTITY_VERIFIED")
		default:
			return observation("ready", stage, "ACCESS_READY")
		}
	}
	var active *registry.ActiveOperationError
	var failure *lifecycle.Failure
	var fault *workerclient.Fault
	switch {
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrForbidden), errors.Is(err, registry.ErrUnauthorized):
		return Unavailable(stage, "AUTHORIZATION_REQUIRED")
	case errors.Is(err, context.Canceled):
		return observation("unknown", stage, "ACCESS_CANCELED")
	case errors.Is(err, registry.ErrGeneration), errors.Is(err, registry.ErrRevision), errors.Is(err, registry.ErrStaleObservation):
		return observation("stale", stage, "SERVICE_IDENTITY_CHANGED")
	case errors.As(err, &active):
		obs := Unavailable(stage, "OPERATION_IN_PROGRESS")
		obs.OperationID = active.OperationID
		return obs
	case errors.Is(err, registry.ErrMaintenance):
		return Unavailable(stage, "MAINTENANCE")
	case errors.As(err, &failure) && failure.Code == lifecycle.StatePointWakeFailed:
		return Unavailable(stage, failure.Code)
	case stage == StageWorkerUpstream:
		return Unavailable(stage, upstreamReason(err))
	case errors.As(err, &fault) && (fault.Code == "E_STALE_EXECUTION" || fault.Code == "E_STALE_REVISION"):
		return observation("stale", stage, "SERVICE_IDENTITY_CHANGED")
	case stage == StageWake:
		if errors.Is(err, context.DeadlineExceeded) {
			return Unavailable(stage, "WAKE_TIMEOUT")
		}
		return Unavailable(stage, "WAKE_ACCESS_UNAVAILABLE")
	case stage == StageGuestManagement:
		return Unavailable(stage, "GUEST_MANAGEMENT_UNAVAILABLE")
	default:
		return Unavailable(stage, "SERVICE_ACCESS_UNAVAILABLE")
	}
}

func upstreamReason(err error) string {
	var netError net.Error
	var certificate *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var alert tls.AlertError
	var fault *workerclient.Fault
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "UPSTREAM_CONNECTION_REFUSED"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netError) && netError.Timeout():
		return "UPSTREAM_TIMEOUT"
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &certificate), errors.As(err, &header), errors.As(err, &alert):
		return "UPSTREAM_TLS_FAILED"
	case errors.As(err, &fault):
		switch fault.Code {
		case "E_STALE_EXECUTION", "E_STALE_REVISION":
			return "SERVICE_IDENTITY_CHANGED"
		case "E_PERMISSION_DENIED":
			return "AUTHORIZATION_REQUIRED"
		}
	}
	return "UPSTREAM_UNAVAILABLE"
}

func Probe(err error) Observation {
	if err == nil {
		return observation("ready", StageWorkerUpstream, "READINESS_PROBE_PASSED")
	}
	var failure *lifecycle.Failure
	if errors.As(err, &failure) && failure.Code == "SERVICE_NOT_READY" {
		return Unavailable(StageWorkerUpstream, "UPSTREAM_UNEXPECTED_STATUS")
	}
	return Diagnose(StageWorkerUpstream, err)
}

type Diagnostic struct {
	Observation Observation
	cause       error
}

func (d *Diagnostic) Error() string { return d.cause.Error() }
func (d *Diagnostic) Unwrap() error { return d.cause }

func Annotate(err error, obs Observation) error {
	if err == nil {
		return nil
	}
	var failure *lifecycle.Failure
	if obs.Stage == StageWorkerUpstream && obs.Recordable() && !errors.As(err, &failure) {
		err = &rt.Error{Code: "UPSTREAM_UNAVAILABLE", Status: http.StatusBadGateway, Message: "service backend unavailable", Cause: err}
	}
	return &Diagnostic{Observation: obs, cause: err}
}
