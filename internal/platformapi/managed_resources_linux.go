package platformapi

import (
	"context"
	"encoding/json"

	"sandbox-platform/internal/lifecycle"
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
	if err != nil {
		if workerclient.RetryDelivery(err) {
			return receipt, err
		}
		if failure := resourceFault(err); failure != nil {
			return receipt, failure
		}
		j.Written = true
	}
	return receipt, err
}
