package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type createRuntime struct {
	rt.Adapter
	creates []rt.CreateRequest
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
	for _, request := range client.creates {
		if request.Name == name {
			return rt.Observation{Name: name, State: rt.StateCreated, RawState: rt.StateCreated, Labels: request.Labels}, nil
		}
	}
	return rt.Observation{}, rt.ErrNotFound
}

func TestDriveResolvesStoredBindingBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	official := &createRuntime{}
	engine := New(db, rt.NewResolver(official, nil), config.Defaults())

	for _, name := range []string{"legacy", "custom"} {
		t.Run(name, func(t *testing.T) {
			op, _, err := db.Accept(ctx, registry.AcceptRequest{
				PrincipalID: owner.ID,
				Epoch:       control.ControlEpoch,
				Key:         "create-" + name,
				Scope:       "POST /v1/machines",
				Kind:        "create",
				TemplateID:  "t1",
				Name:        name,
				Request:     JSON(Create{Name: name, TemplateID: "t1"}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if name == "custom" {
				_, err := db.DB().ExecContext(ctx,
					"INSERT INTO runtime_bundles VALUES ('bundle', 'digest', '{}', 'verified', 'approved', 1);"+
						"INSERT INTO runtime_instances VALUES ('serve', 'deep', 'bundle', 'isolated-socket', 'ready', 1);"+
						"UPDATE runtime_bindings SET runtime_instance_id='serve', runtime_bundle_id='bundle' WHERE id=?;", op.BindingID)
				if err != nil {
					t.Fatal(err)
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
