package operations

import (
	"context"

	"sandbox-platform/internal/registry"
)

type AcceptRequest = registry.AcceptRequest
type TransitionRequest = registry.TransitionRequest

type Service struct {
	store *registry.Store
}

func New(store *registry.Store) *Service { return &Service{store: store} }

func (s *Service) Accept(ctx context.Context, request AcceptRequest) (registry.Operation, bool, error) {
	return s.store.Accept(ctx, request)
}

func (s *Service) Transition(ctx context.Context, id string, request TransitionRequest) (registry.Operation, error) {
	return s.store.Transition(ctx, id, request)
}

func (s *Service) Cancel(ctx context.Context, id string) (registry.Operation, error) {
	return s.store.Cancel(ctx, id)
}

func (s *Service) ObserveMachine(ctx context.Context, bindingID string, observation registry.Observation) error {
	return s.store.ObserveMachine(ctx, bindingID, observation)
}
