package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type resourceSample struct {
	CPUs *struct {
		Created  uint8 `json:"created"`
		Capacity uint8 `json:"capacity"`
	} `json:"cpus"`
	Online *uint8 `json:"online_cpus"`
	RAM    *struct {
		BootMiB  uint32 `json:"boot_mib"`
		Mapped   uint64 `json:"mapped"`
		Plugged  uint64 `json:"plugged"`
		Capacity uint64 `json:"capacity"`
	} `json:"ram"`
	MemoryMiB *uint32 `json:"memory_mib"`
	Balloon   *struct {
		Target *uint32 `json:"target_mib"`
		Actual *uint32 `json:"actual_mib"`
	} `json:"balloon"`
}

type resourceStep struct {
	Change    workerclient.ResourceChange `json:"change"`
	Unchanged json.RawMessage             `json:"unchanged,omitempty"`
}

type managedResourcePlan struct {
	Execution workerclient.Execution          `json:"execution"`
	Steps     []resourceStep                  `json:"steps"`
	Confirmed []workerclient.ResourceProgress `json:"confirmed"`
}

func requireResource(cap workerclient.ComputeCapability) error {
	if cap.Available && cap.Supported {
		return nil
	}
	code, message := "CAPABILITY_UNSUPPORTED", "the running profile does not support this resource change"
	if cap.Supported {
		code = "RESOURCE_NOT_READY"
	}
	if cap.Reason != nil {
		message = *cap.Reason
	}
	return &lifecycle.Failure{Code: code, Message: message}
}

func (r machineResourcePatch) steps(compute workerclient.ComputeResources, limit *uint32) ([]resourceStep, error) {
	var seen resourceSample
	if err := json.Unmarshal(compute.Observed, &seen); err != nil {
		return nil, err
	}
	if len(compute.Pending) > 0 && string(compute.Pending) != "null" {
		return nil, &lifecycle.Failure{Code: "OPERATION_IN_PROGRESS", Message: "another resource change owns the execution"}
	}
	var steps []resourceStep
	add := func(change workerclient.ResourceChange, capability string, unchanged bool) error {
		step := resourceStep{Change: change}
		if unchanged {
			step.Unchanged = compute.Observed
		} else if err := requireResource(compute.Capabilities[capability]); err != nil {
			return err
		}
		steps = append(steps, step)
		return nil
	}
	if r.CPU != 0 {
		if seen.Online == nil || seen.CPUs == nil {
			return nil, &lifecycle.Failure{Code: "GUEST_MANAGEMENT_UNAVAILABLE", Message: "the current Guest online CPU set could not be measured"}
		}
		if r.CPU > seen.CPUs.Capacity {
			return nil, &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "the requested CPU count exceeds this execution's slot capacity"}
		}
		kind := "cpu_online"
		if r.CPU < *seen.Online {
			kind = "cpu_offline"
		}
		capability := kind
		if r.CPU > seen.CPUs.Created {
			capability = "cpu_grow"
		}
		if err := add(workerclient.ResourceChange{Kind: kind, CPUs: r.CPU}, capability, r.CPU == *seen.Online); err != nil {
			return nil, err
		}
	}
	if limit != nil && seen.CPUs != nil && *limit > uint32(max(seen.CPUs.Created, r.CPU))*1000 {
		return nil, &lifecycle.Failure{Code: "INVALID_ARGUMENT", Message: "the requested CPU bandwidth exceeds allocated CPU slots"}
	}
	if r.Memory != 0 {
		ram := seen.RAM
		if ram == nil && seen.MemoryMiB != nil && *seen.MemoryMiB == r.Memory {
			if err := add(workerclient.ResourceChange{Kind: "memory_grow", MemoryMiB: r.Memory}, "memory_grow", true); err != nil {
				return nil, err
			}
		} else if ram == nil {
			if err := requireResource(compute.Capabilities["memory_grow"]); err != nil {
				return nil, err
			}
			return nil, &lifecycle.Failure{Code: "RESOURCE_NOT_READY", Message: "the current RAM allocation could not be measured"}
		} else {
			if r.Memory < ram.BootMiB {
				return nil, &lifecycle.Failure{Code: "INVALID_ARGUMENT", Message: "the RAM target is below the boot allocation"}
			}
			growth := uint64(r.Memory-ram.BootMiB) << 20
			if growth < ram.Mapped || growth > ram.Capacity || growth%(128<<20) != 0 {
				return nil, &lifecycle.Failure{Code: "INVALID_ARGUMENT", Message: "RAM grows in 128 MiB increments within the measured capacity"}
			}
			if err := add(workerclient.ResourceChange{Kind: "memory_grow", MemoryMiB: r.Memory}, "memory_grow", growth == ram.Mapped && growth == ram.Plugged); err != nil {
				return nil, err
			}
		}
	}
	if r.Balloon != nil {
		total := r.Memory
		if total == 0 && seen.MemoryMiB != nil {
			total = *seen.MemoryMiB
		}
		if (total > 0 && *r.Balloon >= total) || *r.Balloon > ^uint32(0)/256 {
			return nil, &lifecycle.Failure{Code: "INVALID_ARGUMENT", Message: "the balloon target must fit within the allocated Guest RAM"}
		}
		b := seen.Balloon
		if err := add(workerclient.ResourceChange{Kind: "balloon", TargetMiB: r.Balloon}, "balloon",
			b != nil && b.Target != nil && b.Actual != nil && *b.Target == *r.Balloon && *b.Actual == *r.Balloon); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

// Each step advances with its public projection in one registry transaction.
// A lost reply reuses the same Worker operation and the originating execution.
func (s *Server) configureManagedResources(ctx context.Context, j *lifecycle.Job, patch machineResourcePatch, limit *uint32, socket string) (map[string]any, error) {
	if patch.Storage != 0 || patch.Overlay != 0 {
		return nil, &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "managed System disk capacity changes use the volume API"}
	}
	client := workerclient.Client{Socket: socket, Bundle: j.Binding.RuntimeBundleID}
	plan := managedResourcePlan{Execution: lifecycle.WorkerExecution(j.Binding)}
	if j.Intent.Stage == "managed_resources" {
		if err := json.Unmarshal([]byte(j.Intent.Target), &plan); err != nil {
			return nil, err
		}
		if plan.Execution.IncarnationID == "" || len(plan.Confirmed) > len(plan.Steps) {
			return nil, errors.New("invalid persisted resource plan")
		}
	} else {
		if plan.Execution.IncarnationID == "" || plan.Execution.Epoch < 1 {
			return nil, &lifecycle.Failure{Code: "MACHINE_NOT_RUNNING", Message: "live resource changes require a running execution"}
		}
		compute, err := client.ComputeResources(ctx, plan.Execution)
		if err != nil {
			return nil, preflightResourceError(err)
		}
		plan.Steps, err = patch.steps(compute, limit)
		if err != nil {
			return nil, err
		}
		if len(patch.Limit) != 0 {
			if _, err = client.ManagedCPULimit(ctx, plan.Execution); err != nil {
				return nil, preflightResourceError(err)
			}
		}
		if err := j.Stage(ctx, "managed_resources", string(lifecycle.JSON(plan))); err != nil {
			return nil, err
		}
	}
	for len(plan.Confirmed) < len(plan.Steps) {
		step := plan.Steps[len(plan.Confirmed)]
		change := step.Change
		progress := workerclient.ResourceProgress{Change: change, Stage: "unchanged", Observed: step.Unchanged}
		if step.Unchanged == nil {
			receipt, err := client.Resize(ctx, j.Op.ID+"-"+change.Kind, plan.Execution, change)
			if err != nil {
				return nil, settleResourceError(ctx, j, plan, err, nil)
			}
			if receipt.Phase == "NEW" && receipt.Decision == "NONE" {
				return nil, nil
			}
			if receipt.Resize == nil {
				return nil, fmt.Errorf("%w: the Worker omitted the resource outcome", rt.ErrUncertain)
			}
			progress = *receipt.Resize
			if receipt.Phase == "ABORTED" && receipt.Decision == "ABORT" && progress.Stage == "aborted" && progress.ErrorCode != "" {
				return nil, settleResourceError(ctx, j, plan, &workerclient.Fault{Code: progress.ErrorCode, Message: progress.Error}, &progress)
			}
			if receipt.Phase != "COMPLETED" || receipt.Decision != "NONE" || progress.Stage != "completed" || !verifiedResourceChange(change, progress.Observed) {
				return nil, fmt.Errorf("%w: the Worker could not verify the resource outcome", rt.ErrUncertain)
			}
		}
		plan.Confirmed = append(plan.Confirmed, progress)
		if progress.Stage == "completed" {
			j.Written = true
		}
		var projection map[string]any
		switch change.Kind {
		case "cpu_online", "cpu_offline":
			projection = map[string]any{"cpu_count": change.CPUs}
		case "memory_grow":
			projection = map[string]any{"memory_mib": change.MemoryMiB}
		case "balloon":
			projection = map[string]any{"balloon_target_mib": change.TargetMiB}
		default:
			return nil, errors.New("invalid persisted resource change")
		}
		err := j.StageApply(ctx, "managed_resources", string(lifecycle.JSON(plan)), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE machines SET spec_json=json_set(spec_json,'$.resources',json_patch(COALESCE(json_extract(spec_json,'$.resources'),'{}'),?)) WHERE id=?",
				string(lifecycle.JSON(projection)), j.Op.MachineID)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	result := map[string]any{"changes": plan.Confirmed, "execution": plan.Execution}
	if len(patch.Limit) != 0 {
		receipt, err := client.SetManagedCPULimit(ctx, j.Op.ID+"-cpu-limit", plan.Execution, limit)
		if err != nil {
			return nil, settleResourceError(ctx, j, plan, err, nil)
		}
		result["cpu_limit"] = receipt
	}
	return result, nil
}

func verifiedResourceChange(change workerclient.ResourceChange, observed json.RawMessage) bool {
	var seen resourceSample
	if json.Unmarshal(observed, &seen) != nil {
		return false
	}
	switch change.Kind {
	case "cpu_online", "cpu_offline":
		return seen.Online != nil && *seen.Online == change.CPUs && seen.CPUs != nil && seen.CPUs.Created >= change.CPUs
	case "memory_grow":
		return seen.MemoryMiB != nil && *seen.MemoryMiB == change.MemoryMiB && seen.RAM != nil &&
			seen.RAM.Mapped == seen.RAM.Plugged && (uint64(seen.RAM.BootMiB)<<20)+seen.RAM.Plugged == uint64(change.MemoryMiB)<<20
	case "balloon":
		return change.TargetMiB != nil && seen.Balloon != nil && seen.Balloon.Target != nil && seen.Balloon.Actual != nil &&
			*seen.Balloon.Target == *change.TargetMiB && *seen.Balloon.Actual == *change.TargetMiB
	}
	return false
}

func preflightResourceError(err error) error {
	if failure := resourceFault(err); failure != nil && !workerclient.RetryDelivery(err) {
		return failure
	}
	return err
}

func managedSettingsError(ctx context.Context, j *lifecycle.Job, err error) error {
	if err == nil || workerclient.RetryDelivery(err) {
		return nil
	}
	var failure *lifecycle.Failure
	if errors.As(err, &failure) {
		return failResourceChange(ctx, j, managedResourcePlan{Execution: lifecycle.WorkerExecution(j.Binding)}, failure, nil)
	}
	return err
}

func settleResourceError(ctx context.Context, j *lifecycle.Job, plan managedResourcePlan, err error, rejected *workerclient.ResourceProgress) error {
	if rejected == nil && workerclient.RetryDelivery(err) {
		return nil
	}
	failure := resourceFault(err)
	if failure == nil {
		return fmt.Errorf("%w: %v", rt.ErrUncertain, err)
	}
	return failResourceChange(ctx, j, plan, failure, rejected)
}

func failResourceChange(ctx context.Context, j *lifecycle.Job, plan managedResourcePlan, failure *lifecycle.Failure, rejected *workerclient.ResourceProgress) error {
	effect := registry.EffectNone
	result := map[string]any{"machine_id": j.Op.MachineID, "execution": plan.Execution, "changes": plan.Confirmed, "rejected": rejected}
	for _, step := range plan.Confirmed {
		if step.Stage == "completed" {
			effect = registry.EffectPartial
		}
	}
	var patch patchMachine
	if err := json.Unmarshal(j.Op.Request, &patch); err != nil {
		return err
	}
	if patch.Policy != nil || patch.Keep != nil || patch.Idle != nil {
		policy, err := j.DB.LifecyclePolicy(ctx, j.Op.MachineID)
		if err != nil {
			return err
		}
		if policy.LastOperation == j.Op.ID && policy.AppliedRevision == policy.DesiredRevision {
			effect = registry.EffectPartial
			result["applied_lifecycle_revision"] = policy.AppliedRevision
		}
	}
	return j.Failed(ctx, registry.TransitionRequest{Effect: effect, Result: lifecycle.JSON(result),
		Error: lifecycle.JSON(resourceIssue{failure.Code, failure.Message})})
}
