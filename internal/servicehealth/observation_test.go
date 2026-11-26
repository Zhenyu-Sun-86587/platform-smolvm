package servicehealth

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"syscall"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func TestServiceObservationsAreFencedToRevisionAndExecution(t *testing.T) {
	var tracker Tracker
	first := Identity{ServiceRevision: 1, BindingID: "binding", BindingGeneration: 1, IncarnationID: "old", ExecutionEpoch: 1}
	if obs := tracker.View("service", first, true, true); obs.Status != "unknown" || obs.ObservedAt != nil {
		t.Fatalf("configured service = %#v", obs)
	}
	tracker.Record("service", first, Probe(nil))
	if obs := tracker.View("service", first, true, true); obs.Status != "ready" || obs.Scope != "worker_upstream" || obs.ObservedAt == nil {
		t.Fatalf("upstream observation = %#v", obs)
	}
	if obs := tracker.View("service", first, true, false); obs.Status != "stale" || obs.ReasonCode != "EXECUTION_NOT_RUNNING" {
		t.Fatalf("sleeping service = %#v", obs)
	}
	newRevision := first
	newRevision.ServiceRevision = 2
	if obs := tracker.View("service", newRevision, true, true); obs.Status != "stale" || obs.ServiceRevision != first.ServiceRevision {
		t.Fatalf("service revision changed = %#v", obs)
	}
	resumed := newRevision
	resumed.ExecutionEpoch++
	resumed.IncarnationID = "resumed"
	tracker.Record("service", resumed, Probe(syscall.ECONNREFUSED))
	tracker.Record("service", first, Probe(nil))
	if obs := tracker.View("service", resumed, true, true); obs.Status != "unavailable" || obs.ReasonCode != "UPSTREAM_CONNECTION_REFUSED" || obs.ExecutionEpoch != resumed.ExecutionEpoch {
		t.Fatalf("late observation changed the current execution's evidence: %#v", obs)
	}
}

func TestServiceObservationUsesWakeAndAgesEvidence(t *testing.T) {
	var tracker Tracker
	key := Identity{ServiceRevision: 1, BindingID: "binding", BindingGeneration: 1, IncarnationID: "saved", ExecutionEpoch: 7}
	tracker.Record("service", key, WakePending())
	if obs := tracker.View("service", key, true, false); obs.Status != "unknown" || obs.Stage != StageWake {
		t.Fatalf("explicit access to sleeping service = %#v", obs)
	}
	if obs := tracker.View("service", key, false, false); obs.Status != "unavailable" || obs.Stage != StageConfiguration || obs.ReasonCode != "SERVICE_DISABLED" {
		t.Fatalf("disabled service = %#v", obs)
	}
	old := time.Now().Add(-freshFor - time.Minute)
	sample := Probe(nil)
	sample.ObservedAt = &old
	tracker.Record("other-service", key, sample)
	if obs := tracker.View("other-service", key, true, true); obs.Status != "stale" || obs.ReasonCode != "OBSERVATION_EXPIRED" {
		t.Fatalf("expired service evidence = %#v", obs)
	}
}

func TestServiceDiagnosticsRetainActualFailureLayer(t *testing.T) {
	for _, tt := range []struct {
		name       string
		stage      string
		err        error
		reason     string
		recordable bool
	}{
		{"refused", StageWorkerUpstream, syscall.ECONNREFUSED, "UPSTREAM_CONNECTION_REFUSED", true},
		{"tls", StageWorkerUpstream, tls.RecordHeaderError{}, "UPSTREAM_TLS_FAILED", true},
		{"auth", StageWorkerUpstream, auth.ErrForbidden, "AUTHORIZATION_REQUIRED", false},
		{"revision", StageWorkerUpstream, registry.ErrRevision, "SERVICE_IDENTITY_CHANGED", false},
		{"cancelled", StageWorkerUpstream, context.Canceled, "ACCESS_CANCELED", false},
		{"timeout", StageWake, context.DeadlineExceeded, "WAKE_TIMEOUT", true},
		{"guest", StageGuestManagement, rt.ErrUnavailable, "GUEST_MANAGEMENT_UNAVAILABLE", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obs := Diagnose(tt.stage, tt.err)
			if obs.Stage != tt.stage || obs.ReasonCode != tt.reason || obs.Recordable() != tt.recordable {
				t.Fatalf("diagnostic = %#v", obs)
			}
			err := Annotate(tt.err, obs)
			var diagnostic *Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Observation.ReasonCode != tt.reason {
				t.Fatalf("annotated diagnostic = %v", err)
			}
		})
	}
	err := syscall.ECONNREFUSED
	var upstream *rt.Error
	if annotated := Annotate(err, Probe(err)); !errors.Is(annotated, err) || !errors.As(annotated, &upstream) || upstream.Status != http.StatusBadGateway {
		t.Fatalf("upstream response = %v", annotated)
	}
	if obs := Probe(&lifecycle.Failure{Code: "SERVICE_NOT_READY"}); obs.ReasonCode != "UPSTREAM_UNEXPECTED_STATUS" {
		t.Fatalf("unexpected readiness status = %#v", obs)
	}
	if obs := Diagnose(StageWake, &registry.ActiveOperationError{OperationID: "op-real"}); obs.OperationID != "op-real" {
		t.Fatalf("active operation = %#v", obs)
	}
}
