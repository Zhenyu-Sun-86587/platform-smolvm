package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
)

func TestAcceptedCreateSurvivesLostResponse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "platform.db")
	s, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth.New(s).Bootstrap(ctx, "owner", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	err = s.EnsureTemplate(ctx, registry.Template{ID: "t1", Name: "managed", Version: 1, ImageDigest: "alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := registry.AcceptRequest{PrincipalID: p.ID, PrincipalScope: []byte(`{"access":"manage"}`), Kind: "create", Name: "example", TemplateID: "t1", Key: "lost-response", Epoch: ctl.ControlEpoch, Scope: "POST /v1/machines", Heavy: true, Request: []byte(`{"name":"example"}`)}
	op, again, err := s.Accept(ctx, req)
	if err != nil || again {
		t.Fatal(again, err)
	}
	if _, err := registry.Open(ctx, path); !errors.Is(err, registry.ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replay, again, err := s.Accept(ctx, req)
	if err != nil || !again || op.ID != replay.ID || op.RuntimeName != replay.RuntimeName {
		t.Fatal(replay, again, err)
	}
	if _, err = json.Marshal(replay); err != nil {
		t.Fatal(err)
	}
	req.Name = "other"
	if _, _, err = s.Accept(ctx, req); !errors.Is(err, registry.ErrIdempotency) {
		t.Fatal(err)
	}
	ms, err := s.ListMachines(ctx)
	if err != nil || len(ms) != 1 {
		t.Fatalf("machines=%d err=%v", len(ms), err)
	}
}
