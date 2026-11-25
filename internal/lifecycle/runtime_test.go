package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type createRuntime struct {
	rt.Adapter
	creates       []rt.CreateRequest
	starts        int
	startedName   string
	workloadError string
	readError     error
}

func (*createRuntime) Health(context.Context) (rt.Health, error) {
	return rt.Health{Version: rt.Version}, nil
}

func (*createRuntime) Ready(context.Context) error { return nil }

func (client *createRuntime) Create(ctx context.Context, request rt.CreateRequest) (rt.Observation, error) {
	client.creates = append(client.creates, request)
	return client.Observe(ctx, request.Name)
}

func (client *createRuntime) Observe(_ context.Context, name string) (rt.Observation, error) {
	if client.readError != nil && name == client.startedName {
		return rt.Observation{}, client.readError
	}
	return client.observation(name)
}

func (client *createRuntime) Start(_ context.Context, name string) (rt.Receipt, error) {
	client.starts++
	client.startedName = name
	o, err := client.observation(name)
	if client.workloadError != "" {
		o.WorkloadError = &client.workloadError
	}
	return rt.Receipt{Name: name, Observation: o}, err
}

func (client *createRuntime) observation(name string) (rt.Observation, error) {
	for _, request := range client.creates {
		if request.Name == name {
			o := rt.Observation{Name: name, State: rt.StateCreated, RawState: rt.StateCreated, Labels: request.Labels, ObservedAt: time.Now().UTC()}
			if name == client.startedName {
				o.State, o.RawState = rt.StateRunning, rt.StateRunning
				o.Identity = &rt.Identity{PID: json.RawMessage("42"), StartTime: json.RawMessage("3")}
			}
			return o, nil
		}
	}
	return rt.Observation{}, rt.ErrNotFound
}

func runtimeTestEngine(t *testing.T, ctx context.Context, official rt.Adapter) (*Engine, string, string) {
	t.Helper()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	owner, err := auth.New(db).Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "t1", Name: "base", Version: 1, ImageDigest: "alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	control, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(db, rt.NewResolver(official, nil), config.Defaults())
	return engine, owner.ID, control.ControlEpoch
}

func TestDriveResolvesStoredBindingBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	official := &createRuntime{}
	engine, owner, epoch := runtimeTestEngine(t, ctx, official)
	db := engine.DB
	customRoute := rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}
	if err := db.RegisterRuntime(ctx, customRoute, "isolated-socket", JSON(map[string]string{"schema": "RuntimeBundle/1"})); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy", "custom"} {
		t.Run(name, func(t *testing.T) {
			request := registry.AcceptRequest{
				PrincipalID: owner,
				Epoch:       epoch,
				Key:         "create-" + name,
				Scope:       "POST /v1/machines",
				Kind:        "create",
				TemplateID:  "t1",
				Name:        name,
				Request:     JSON(Create{Name: name, TemplateID: "t1"}),
			}
			if name == "custom" {
				request.RuntimeInstanceID = customRoute.InstanceID
				request.AdmitRuntime = engine.AdmitRuntime
				if _, _, err := db.Accept(ctx, request); !errors.Is(err, rt.ErrUnsupported) {
					t.Fatalf("unavailable runtime admitted before dispatch: %v", err)
				}
				// Model a previously admitted operation after its runtime was removed.
				request.AdmitRuntime = func(route rt.BundleRoute, kind string) error {
					if route != customRoute || kind != "create" {
						t.Fatalf("unexpected admission of %v %s", route, kind)
					}
					return nil
				}
			}
			op, _, err := db.Accept(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if name == "custom" {
				binding, err := db.GetBinding(ctx, op.BindingID)
				if err != nil || bindingRuntime(binding).BundleRoute != customRoute {
					t.Fatalf("binding route was not persisted: %+v, %v", binding, err)
				}
				request.AdmitRuntime = engine.AdmitRuntime
				replay, found, err := db.Accept(ctx, request)
				if err != nil || !found || replay.ID != op.ID {
					t.Fatalf("idempotent replay = %+v, %t, %v", replay, found, err)
				}
			}
			if err := engine.drive(ctx, op); err != nil {
				t.Fatal(err)
			}
			result, err := db.GetOperation(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "legacy" {
				if result.State != registry.Succeeded || result.Effect != registry.EffectApplied {
					t.Fatalf("legacy result = %#v", result)
				}
			} else {
				var failure struct {
					Code string
				}
				err := json.Unmarshal(result.Error, &failure)
				if result.State != registry.Failed || result.Effect != registry.EffectNone || err != nil || failure.Code != "NOT_SUPPORTED" {
					t.Fatalf("custom result = %#v, error decode = %v", result, err)
				}
			}
		})
	}
	if len(official.creates) != 1 {
		t.Fatalf("official create requests = %d, want one legacy create", len(official.creates))
	}
}

func TestStartWorkloadFailureSurvivesReconciliation(t *testing.T) {
	const message = "entrypoint failed"
	ctx := context.Background()
	official := &createRuntime{workloadError: message, readError: rt.ErrUnavailable}
	engine, owner, epoch := runtimeTestEngine(t, ctx, official)
	db := engine.DB
	op, _, err := db.Accept(ctx, registry.AcceptRequest{
		PrincipalID: owner, Epoch: epoch, Key: "create-with-start", Scope: "POST /v1/machines",
		Kind: "create", TemplateID: "t1", Name: "example",
		Request: JSON(Create{Name: "example", TemplateID: "t1", Start: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.drive(ctx, op); err != nil {
		t.Fatal(err)
	}
	dispatched, err := db.GetOperation(ctx, op.ID)
	if err != nil || dispatched.State != registry.RecoveryRequired || dispatched.Effect != registry.EffectUnknown {
		t.Fatalf("unobserved start result = %#v, %v", dispatched, err)
	}
	official.readError = nil
	reconcile, err := db.Transition(ctx, op.ID, registry.TransitionRequest{State: registry.Observing, Stage: "reconciling"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.drive(ctx, reconcile); err != nil {
		t.Fatal(err)
	}
	result, err := db.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var failure map[string]string
	if err := json.Unmarshal(result.Error, &failure); err != nil || result.State != registry.Failed || result.Effect != registry.EffectApplied ||
		failure["code"] != registry.WorkloadStartFailed || failure["message"] != message || official.starts != 1 {
		t.Fatalf("reconciled result = %#v, failure = %v, start calls = %d, decode error = %v", result, failure, official.starts, err)
	}
	assertHealth := func() {
		t.Helper()
		m, err := db.GetMachine(ctx, op.MachineID)
		if err != nil || m.CurrentBinding == nil || m.CurrentBinding.ID != op.BindingID || m.ObservedState != rt.StateRunning || m.DesiredPower != rt.StateRunning {
			t.Fatalf("machine = %#v, %v", m, err)
		}
		var health struct {
			Workload string            `json:"workload"`
			Failure  map[string]string `json:"workload_error"`
		}
		if err := json.Unmarshal(m.CurrentBinding.Health, &health); err != nil || health.Workload != "failed" || health.Failure["message"] != message {
			t.Fatalf("health = %#v, %v", health, err)
		}
	}
	assertHealth()
	o, err := official.Observe(ctx, op.RuntimeName)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveMachine(ctx, op.BindingID, Evidence(o)); err != nil {
		t.Fatal(err)
	}
	assertHealth()
}
