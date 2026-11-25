package platformapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

type rollbackFence struct {
	binding  registry.Binding
	state    string
	artifact string
}

func (f rollbackFence) evidence() string { return f.binding.RuntimeName + ":" + f.state }

func (s *Server) fenceOld(ctx context.Context, j *lifecycle.Job, old registry.Binding, policy string) (rollbackFence, error) {
	fence := rollbackFence{binding: old}
	observed, err := s.observeOld(ctx, old)
	if err != nil {
		j.Written = true
		return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old execution could not be observed: " + err.Error()}
	}
	switch observed.State {
	case rt.StateRunning:
		if !observed.Identity.Complete() {
			return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old running execution has incomplete identity"}
		}
		if err := s.checkRestartPolicy(ctx, old, observed.State); err != nil {
			return fence, err
		}
		id, err := s.capture(ctx, j, old, "recovery", "pre-replacement execution", false)
		if err != nil {
			return fence, err
		}
		fence.artifact = id
		if err := s.Snapshots.Pin(ctx, id, "pre_replacement", j.Op.ID); err != nil {
			return fence, err
		}
		latest, err := s.observeOld(ctx, old)
		if err != nil {
			return fence, err
		}
		if latest.State != rt.StateRunning || !latest.Identity.Equal(observed.Identity) {
			return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old running execution changed before Stop"}
		}
		if err := j.Stage(ctx, "stop_old", ""); err != nil {
			return fence, err
		}
		if _, err := s.Runtime.Stop(ctx, old.RuntimeName); err != nil {
			return fence, err
		}
		j.Written = true
		fence.state = rt.StateStopped
	case rt.StatePaused:
		if policy != "export_then_delete_on_paused" {
			return fence, &lifecycle.Failure{Code: "PRECONDITION_FAILED", Message: "paused rollback requires export_then_delete_on_paused"}
		}
		id, err := s.capture(ctx, j, old, "recovery", "pre-replacement saved execution", true)
		if err != nil {
			// A pressure-paused execution can auto-resume while it is being
			// exported. Hold the original operation for observation.
			j.Written = true
			return fence, err
		}
		fence.artifact = id
		if err := s.Snapshots.Pin(ctx, id, "pre_replacement", j.Op.ID); err != nil {
			return fence, err
		}
		latest, err := s.observeOld(ctx, old)
		if err != nil {
			return fence, err
		}
		if latest.State != rt.StatePaused || latest.StateReason != observed.StateReason {
			return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "saved execution changed after export"}
		}
		if err := j.Stage(ctx, "delete_old", ""); err != nil {
			return fence, err
		}
		if _, err := s.Runtime.Delete(ctx, old.RuntimeName); err != nil {
			return fence, err
		}
		fence.state = "absent"
	case rt.StateCreated, rt.StateStopped:
		if old.PauseStatus == "active" {
			j.Written = true
			return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old binding retains a saved-execution receipt"}
		}
		fence.state = observed.State
	default:
		j.Written = true
		return fence, &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old execution cannot be fenced from state " + observed.State}
	}
	return fence, s.verifyFence(ctx, fence)
}

func (s *Server) verifyFence(ctx context.Context, fence rollbackFence) error {
	observed, err := s.observeOld(ctx, fence.binding)
	if fence.state == "absent" {
		if errors.Is(err, rt.ErrNotFound) {
			return nil
		}
		return &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old saved execution still present or unreachable"}
	}
	if err != nil {
		return err
	}
	if !matchesInactiveFence(observed, fence.state) {
		return &lifecycle.Failure{Code: "RECOVERY_REQUIRED", Message: "old execution fence no longer matches fresh runtime state"}
	}
	return s.checkRestartPolicy(ctx, fence.binding, fence.state)
}

func matchesInactiveFence(observed rt.Observation, expected string) bool {
	return (expected == rt.StateCreated || expected == rt.StateStopped) && observed.State == expected &&
		observed.Identity == nil && !observed.PressurePaused()
}

func (s *Server) observeOld(ctx context.Context, old registry.Binding) (rt.Observation, error) {
	o, err := s.Runtime.Observe(ctx, old.RuntimeName)
	if err == nil && (o.Labels["platform.binding"] != old.ID || o.Labels["platform.machine"] != old.MachineID || o.Labels["platform.spec"] != old.SpecHash) {
		err = registry.ErrStaleObservation
	}
	return o, err
}

func (s *Server) checkRestartPolicy(ctx context.Context, binding registry.Binding, state string) error {
	if err := smolvm.CheckRestartNever(ctx, s.Config.Runtime.StatusSocket, binding.RuntimeName, binding.ID, state); err != nil {
		return &lifecycle.Failure{Code: "RESTART_POLICY_UNVERIFIED", Message: fmt.Sprintf("restart policy evidence for %s is unavailable: %v", binding.RuntimeName, err)}
	}
	return nil
}

func candidateReady(ctx context.Context, until time.Duration, check func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, until)
	defer cancel()
	var last error
	for {
		last = check(ctx)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(150 * time.Millisecond):
		}
	}
}
