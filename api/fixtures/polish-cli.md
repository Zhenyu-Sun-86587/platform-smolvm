# CLI product contract

The [command reference](../../cmd/sandboxctl/USAGE.md) documents command
names and script behavior. The CLI uses the existing Phase 2 G0 wire
and the following opt-in guards.

- `machines wake-full --from standby` submits `/wake` with
  `expected_power_state:"standby"`. `--from hibernated` submits `/start`
  with `expected_power_state:"hibernated"`. `machines cold-start` submits
  `/start` with `expected_power_state:"stopped"`. These calls carry
  `expected_generation`, check the power state on acceptance and use the
  resulting native or cold path. A changed power state returns
  `STATE_CONFLICT`. An official binding cannot satisfy a native saved-state
  guard.
- `snapshots capture-vm` submits `capture_mode:"vm"`.
  `restore-vm` and `rollback-vm` submit `require_capture_mode:"vm"`.
  `CAPTURE_MODE_CONFLICT` reports a managed-task capture offered to either
  VM-only operation. The established `volume_restore_policy` defaults to
  `clone_conflicts`; `exact` retains its existing conflict checks.
- `machines live-fork` and `machines fork-batch` always request
  `require_independent:true`. The batch can pass `admission` and
  `volume_policy.default:"clone"`. The original compatibility choices
  remain available on the original commands.

[Fixed script examples](polish-cli.json) show structured conflicts, timeouts,
mutation uncertainty and partial fork receipts.

The existing operation states and effects retain their wire meaning.
Use `--strict-result` to make partial or unknown effects return nonzero.
JSON mutation errors report the original intent in `error.intent`, while
`OPERATION_WAIT_TIMEOUT` reports the original operation ID and exits 124.
The client disables body replay for transmitted mutations, including empty
writes. Receipt recovery stays an explicit caller decision.
An operation that ends in `succeeded` with `effect:"partial"` can have both
successful and failed children. Inspect its `children` receipts and use
`operations retry` for eligible failed targets on the same operation ID.
