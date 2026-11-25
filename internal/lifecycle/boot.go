package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func hostBootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func stoppedAfterHostReboot(bootID string, binding registry.Binding, current rt.Observation) bool {
	if bootID == "" || current.State != rt.StateStopped || current.RawState != rt.StateStopped ||
		current.Identity != nil || binding.PauseStatus == "active" || binding.PauseStatus == "unknown" {
		return false
	}
	var prior rt.Identity
	if json.Unmarshal(binding.RuntimeIdentity, &prior) != nil || !prior.Complete() {
		return false
	}
	var priorBoot string
	return json.Unmarshal(prior.BootID, &priorBoot) == nil && priorBoot != "" && priorBoot != bootID
}

// recordHostBootLoss keeps terminal guest receipts authoritative. A different
// host boot and a fresh native stopped state disprove continuity but cannot
// distinguish an unobserved Guest exit from interrupted execution.
func (e *Engine) recordHostBootLoss(ctx context.Context, binding registry.Binding, observedAt time.Time) error {
	_, err := e.DB.DB().ExecContext(ctx, `
		UPDATE tasks SET state = 'unknown', observed_at = ?,
			result_json = json_set(result_json, '$.state', 'unknown', '$.reason',
				'host_boot_changed', '$.observed_at', ?)
		WHERE machine_id = ? AND binding_generation = ? AND task_namespace = ?
			AND state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted', 'unknown')
			AND EXISTS (
				SELECT 1 FROM machines m JOIN runtime_bindings b ON m.current_binding_id = b.id
				WHERE m.id = tasks.machine_id AND m.deleted_at IS NULL AND b.id = ?
					AND b.runtime_identity_json = ?
					AND NOT EXISTS (SELECT 1 FROM operation_locks busy WHERE busy.machine_id = m.id)
			)`, observedAt.UnixMilli(), observedAt.UTC().Format(time.RFC3339Nano),
		binding.MachineID, binding.Generation, binding.TaskNamespace, binding.ID, string(binding.RuntimeIdentity))
	return err
}
