package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/execbridge"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type guestExec struct {
	execution rt.ExecutionRef
	session   *execbridge.GuestSession
}

func (s *Server) defaultExecDirectory(ctx context.Context, machine string, spec *execRequest) error {
	m, err := s.DB.GetMachine(ctx, machine)
	if err != nil {
		return err
	}
	spec.Cwd = "/workspace"
	if b := m.CurrentBinding; b != nil && b.RuntimeBundleID != "" {
		var launch struct {
			BootMode rt.BootMode `json:"boot_mode"`
		}
		if err := json.Unmarshal(m.Spec, &launch); err != nil {
			return err
		}
		if launch.BootMode == rt.BootSystem {
			spec.Cwd = "/"
		}
	}
	return nil
}

func (s *Server) dispatchGuestExec(ctx context.Context, p auth.Principal, x execRecord, b registry.Binding, adapter rt.Adapter,
	request rt.ExecSessionRequest, stream *execbridge.Stream, release func()) (err error) {
	defer func() {
		if err != nil {
			message := err.Error()
			stream.Observe(rt.ExecSession{State: "failed", Done: true, Error: &message})
			view, _ := stream.Read(0, 0)
			s.saveExec(x.ID, view)
			s.execStreams.CompareAndDelete(x.ID, stream)
			release()
		}
	}()
	client, ok := lifecycle.PolicyClient(adapter, b)
	if !ok || b.PowerState != registry.PowerRunning {
		return rt.ErrUnsupported
	}
	conn, err := client.OpenGuestAgent(ctx, lifecycle.WorkerExecution(b))
	if err != nil {
		return guestExecError(err)
	}
	if err = s.CheckAccessPrincipal(ctx, p, x.Machine); err != nil {
		conn.Close()
		return err
	}
	// After admission the command belongs to this public session. Losing the
	// HTTP reply must not terminate a successfully started command.
	startup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
	defer cancel()
	session, err := execbridge.StartGuest(startup, conn, stream, request)
	if err != nil {
		return guestExecError(err)
	}
	active := &guestExec{execution: b.ExecutionRef(), session: session}
	s.guestExecs.Store(x.ID, active)
	if err := s.markGuestExecRunning(startup, x, b); err != nil {
		s.guestExecs.CompareAndDelete(x.ID, active)
		session.Close("Guest exec execution is no longer current")
		return err
	}
	go func() {
		defer release()
		defer time.AfterFunc(5*time.Minute, func() { s.execStreams.CompareAndDelete(x.ID, stream) })
		defer s.guestExecs.CompareAndDelete(x.ID, active)
		session.Run()
		view, _ := stream.Read(0, 0)
		s.saveExec(x.ID, view)
	}()
	if request.WaitSeconds > 0 {
		wait, stop := context.WithTimeout(ctx, time.Duration(request.WaitSeconds)*time.Second)
		defer stop()
		for {
			view, changed := stream.Read(0, 0)
			if view.Done || view.End != 0 {
				break
			}
			select {
			case <-wait.Done():
				return nil
			case <-changed:
			}
		}
	}
	return nil
}

func (s *Server) markGuestExecRunning(ctx context.Context, x execRecord, b registry.Binding) error {
	result, err := s.DB.DB().ExecContext(ctx, `UPDATE exec_sessions SET state='running',last_observed_at=?
		WHERE id=? AND state='starting' AND EXISTS (SELECT 1 FROM machines m JOIN runtime_bindings b ON b.id=m.current_binding_id
		WHERE m.id=? AND m.deleted_at IS NULL AND b.id=? AND b.incarnation_id=? AND b.execution_epoch=? AND b.power_state='running')`,
		time.Now().UnixMilli(), x.ID, x.Machine, b.ID, b.IncarnationID, b.ExecutionEpoch)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return registry.ErrGeneration
	}
	return nil
}

func (s *Server) currentGuestExec(ctx context.Context, x execRecord) (*execbridge.GuestSession, error) {
	value, ok := s.guestExecs.Load(x.ID)
	if !ok {
		return nil, execLost()
	}
	active := value.(*guestExec)
	m, err := s.DB.GetMachine(ctx, x.Machine)
	if err != nil {
		return nil, err
	}
	b := m.CurrentBinding
	if m.DeletedAt != nil || b == nil || b.ID != x.Binding || b.RuntimeBundleID != x.Bundle ||
		b.ExecutionRef() != active.execution || b.PowerState != registry.PowerRunning {
		active.session.Close("Guest exec execution is no longer current")
		return nil, execLost()
	}
	return active.session, nil
}

func execLost() error {
	return &lifecycle.Failure{Code: "SESSION_LOST", Message: "native exec session has ended or is unavailable"}
}

func guestExecError(err error) error {
	var fault *workerclient.Fault
	switch {
	case err == nil, errors.Is(err, rt.ErrInvalid), errors.Is(err, rt.ErrUnsupported):
		return err
	case errors.Is(err, execbridge.ErrGuestStart):
		return &lifecycle.Failure{Code: "EXEC_START_FAILED", Message: err.Error()}
	case errors.Is(err, execbridge.ErrGuestCapacity):
		return &lifecycle.Failure{Code: "RESOURCE_EXHAUSTED", Message: err.Error()}
	case errors.Is(err, execbridge.ErrStdinSequence):
		return &lifecycle.Failure{Code: "STDIN_SEQUENCE_CONFLICT", Message: err.Error()}
	case errors.Is(err, rt.ErrNotFound):
		return execLost()
	case errors.As(err, &fault):
		switch fault.Code {
		case "E_CAPABILITY_UNSUPPORTED", "E_UNSUPPORTED_METHOD", "E_INVALID_REQUEST", "E_NATIVE_UNBOUND":
			return fmt.Errorf("%w: %s", rt.ErrUnsupported, fault.Message)
		case "E_STALE_EXECUTION", "E_STATE_CONFLICT", "E_NOT_FOUND":
			return execLost()
		case "E_PERMISSION_DENIED":
			return auth.ErrForbidden
		}
	}
	return fmt.Errorf("%w: Guest Agent streaming connection failed", rt.ErrUnavailable)
}
