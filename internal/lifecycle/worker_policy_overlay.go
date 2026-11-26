package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

// Observe the actual Worker wire revision before applying the current desired
// policy and overlay. An RPC retry matches the same base and desired revisions
// and does not replace an already-applied, newer user's policy.
func (e *Engine) applyWorkerPolicyOverlay(ctx context.Context, b registry.Binding, client policyWorker, plan registry.WorkerPolicyOverlay) (registry.LifecycleSettings, error) {
	var current policyReceipt
	scope := WorkerExecution(b)
	err := client.Lifecycle(ctx, "GetLifecycleStatus", "", scope, map[string]any{}, &current)
	var fault *workerclient.Fault
	if err != nil && (!errors.As(err, &fault) || fault.Code != "E_NOT_FOUND") {
		return registry.LifecycleSettings{}, err
	}
	if err == nil && (current.Instance != scope.InstanceID || current.Applied < 1 || current.Effective == nil) {
		return registry.LifecycleSettings{}, registry.ErrGeneration
	}
	if current.Effective != nil && *current.Effective == plan.Effective {
		return e.DB.RecordWorkerPolicyOverlay(ctx, plan, current.Applied)
	}
	next := current.Applied + 1
	key := fmt.Sprintf("maintenance-policy-%s-%d-%d-%d", b.ID, plan.MaintenanceRevision, plan.DesiredRevision, next)
	if err := deliverPolicy(ctx, client, key, scope, current.Applied, next, JSON(plan.Effective)); err != nil {
		return registry.LifecycleSettings{}, err
	}
	return e.DB.RecordWorkerPolicyOverlay(ctx, plan, next)
}
