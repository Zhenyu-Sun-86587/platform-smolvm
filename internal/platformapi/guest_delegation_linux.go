package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

type guestVerifier func(*http.Request) (string, error)

func guestHandler(verify guestVerifier, run func(http.ResponseWriter, *http.Request, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		secret, err := verify(r)
		if err == nil {
			err = run(w, r, secret)
		}
		if err != nil {
			fail(w, r, err)
		}
	}
}

func (s *Server) guestDelegationRoutes(mux *http.ServeMux, peer auth.AttestedGuestContext, verify guestVerifier) {
	mux.HandleFunc("GET /v1/control", guestHandler(verify, func(w http.ResponseWriter, r *http.Request, secret string) error {
		if _, err := s.Auth.AuthenticateGuestCredential(r.Context(), peer, secret); err != nil {
			return err
		}
		state, err := s.DB.Control(r.Context())
		if err != nil {
			return err
		}
		return write(w, http.StatusOK, map[string]string{"control_epoch": state.ControlEpoch})
	}))
	mux.HandleFunc("POST /v1/machines", guestHandler(verify, func(w http.ResponseWriter, r *http.Request, secret string) error {
		return s.guestCreate(w, r, peer, secret)
	}))
	mux.HandleFunc("POST /v1/machines/{machine}/forks", guestHandler(verify, func(w http.ResponseWriter, r *http.Request, secret string) error {
		return s.guestFork(w, r, peer, secret)
	}))
	mux.HandleFunc("GET /v1/operations/{operation}", guestHandler(verify, func(w http.ResponseWriter, r *http.Request, secret string) error {
		caller, err := s.Auth.AuthenticateGuestCredential(r.Context(), peer, secret)
		if err != nil {
			return err
		}
		if caller.Action != auth.GuestCreate && caller.Action != auth.GuestFork {
			return auth.ErrForbidden
		}
		result, err := s.DB.GuestOperation(r.Context(), r.PathValue("operation"), caller.ID, caller.GrantID)
		if err != nil {
			return err
		}
		return write(w, http.StatusOK, result)
	}))
}

func (s *Server) guestCreate(w http.ResponseWriter, r *http.Request, peer auth.AttestedGuestContext, secret string) error {
	caller, err := s.Auth.AuthenticateGuest(r.Context(), peer, secret, auth.GuestCreate, "")
	if err != nil {
		return err
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := decode(r, &input); err != nil {
		return err
	}
	if !validChildName(input.Name) {
		return registry.ErrInvalid
	}
	limits, err := caller.Limits()
	if err != nil || limits.Create == nil {
		return auth.ErrForbidden
	}
	profile := limits.Create
	template, err := s.DB.GetTemplate(r.Context(), profile.TemplateID)
	if err != nil {
		return err
	}
	boot, err := templates.ReadSpec(template)
	if err != nil {
		return err
	}
	if boot.BootMode != rt.BootSystem {
		return rt.SystemBootUnavailable(false)
	}
	request := lifecycle.Create{Name: input.Name, TemplateID: profile.TemplateID, RuntimeInstanceID: profile.RuntimeInstanceID,
		BootMode: rt.BootSystem, Start: true}
	request.Resources.CPUs, request.Resources.Memory = profile.CPUs, profile.MemoryMiB
	if profile.NetworkID != "" {
		request.Network = &rt.LaunchNetwork{NetworkID: profile.NetworkID, Mode: "native"}
	}
	if err := request.ValidateSystemCreate(); err != nil {
		return err
	}
	admit, err := s.systemCreateAdmission(r.Context(), request.RuntimeInstanceID)
	if err != nil {
		return err
	}
	accept := registry.AcceptRequest{Kind: "create", Name: request.Name, TemplateID: request.TemplateID,
		RuntimeInstanceID: request.RuntimeInstanceID, Heavy: true, DesiredPower: "running", AdmitRuntime: admit,
		ResolveCreateDefaults: s.createDefaults(request, boot, false, true, false)}
	accept.AdmitGuest = s.guestOperationAdmission(peer, secret, caller, int64(profile.MemoryMiB))
	return s.accept(w, r, auth.Principal{ID: caller.ID}, request, accept)
}

func (s *Server) guestFork(w http.ResponseWriter, r *http.Request, peer auth.AttestedGuestContext, secret string) error {
	source := r.PathValue("machine")
	caller, err := s.Auth.AuthenticateGuest(r.Context(), peer, secret, auth.GuestFork, source)
	if err != nil {
		return err
	}
	var input struct {
		ChildName          string `json:"child_name"`
		ExpectedGeneration int64  `json:"expected_generation"`
	}
	if err := decode(r, &input); err != nil {
		return err
	}
	if !validChildName(input.ChildName) || input.ExpectedGeneration < 1 {
		return registry.ErrInvalid
	}
	machine, err := s.DB.GetMachine(r.Context(), source)
	if err != nil {
		return err
	}
	var spec lifecycle.Create
	if err := json.Unmarshal(machine.Spec, &spec); err != nil || spec.Resources.Memory == 0 {
		return registry.ErrStaleObservation
	}
	accept := registry.AcceptRequest{Kind: rt.IndependentFork, MachineID: source,
		ExpectedGeneration: input.ExpectedGeneration, ForkChildren: []string{input.ChildName}, Heavy: true,
		AdmitGuest: s.guestOperationAdmission(peer, secret, caller, int64(spec.Resources.Memory))}
	return s.accept(w, r, auth.Principal{ID: caller.ID}, input, accept)
}

func (s *Server) guestOperationAdmission(peer auth.AttestedGuestContext, secret string, caller auth.GuestPrincipal,
	memoryMiB int64) func(context.Context, *sql.Tx, registry.Operation, bool) error {
	return func(ctx context.Context, tx *sql.Tx, op registry.Operation, replay bool) error {
		return s.Auth.AuthorizeGuestOperation(ctx, tx, peer, secret, caller, op, replay, memoryMiB)
	}
}

func watchGuestAuthority(ctx context.Context, check func(context.Context) error) (context.Context, context.CancelCauseFunc) {
	call, cancel := context.WithCancelCause(ctx)
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-call.Done():
				return
			case <-tick.C:
				if err := check(call); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return call, cancel
}
