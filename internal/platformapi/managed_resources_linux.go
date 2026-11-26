package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

func managedCPULimit(ctx context.Context, j *lifecycle.Job, limit *uint32) (workerclient.CPULimitReceipt, error) {
	var receipt workerclient.CPULimitReceipt
	scope := lifecycle.WorkerExecution(j.Binding)
	if j.Intent.Stage == "managed_resource_cpu_limit" {
		if err := json.Unmarshal([]byte(j.Intent.Target), &scope); err != nil {
			return receipt, err
		}
	} else {
		if scope.IncarnationID == "" || scope.Epoch < 1 {
			return receipt, &lifecycle.Failure{Code: "MACHINE_NOT_RUNNING", Message: "a live managed execution is required to verify the CPU quota"}
		}
		if err := j.Stage(ctx, "managed_resource_cpu_limit", string(lifecycle.JSON(scope))); err != nil {
			return receipt, err
		}
	}
	client := workerclient.Client{Socket: privateServiceWorker(j.Runtime), Bundle: j.Binding.RuntimeBundleID}
	receipt, err := client.SetManagedCPULimit(ctx, j.Op.ID+"-cpu-limit", scope, limit)
	var fault *workerclient.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "E_CAPABILITY_UNSUPPORTED", "E_METHOD_UNSUPPORTED", "E_PROTOCOL_UNSUPPORTED":
			return receipt, &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "the selected Worker does not provide managed CPU quotas"}
		case "E_INVALID_REQUEST", "E_RESOURCE_LIMIT":
			return receipt, registry.ErrInvalid
		case "E_STALE_EXECUTION", "E_FENCE_UNPROVEN":
			return receipt, registry.ErrGeneration
		case "E_IDEMPOTENCY_CONFLICT":
			return receipt, registry.ErrIdempotency
		}
	}
	return receipt, err
}
