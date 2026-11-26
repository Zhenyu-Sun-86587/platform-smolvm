# Phase 2 P0 interface fixtures

The samples in `polish-p0.json` are for development. Operation and lifecycle examples are projections of the fields relevant to the scenario. The complete existing wire types live in the TypeScript SDK and the corresponding Go route and registry types.

| Surface | Existing wire | Additive P0 contract | Implementation |
| --- | --- | --- | --- |
| Machines and lifecycle | Machine power, health, capability map, policy and leases | File capability keys and constraints, complete SDK projections of existing policy and lease fields | B3 |
| Files | Files and files/list routes, upload intent bound to path, SHA-256 and size | Native VM target, 64 MiB upload ceiling, 256 KiB text preview/edit bound, structured uncertainty | B1 transport, B3 routing |
| Metrics | Machine metrics and system capacity routes | Observation metadata and omission of unavailable fields, including absent aggregates | B2 observations, B3 routing |
| Snapshots | Snapshot with capture_mode and restore with volume_restore_policy | capture_summary derived from immutable configuration/evidence | B2 capture, B3 routing |
| Services | Service enabled/readiness configuration and existing service access | Latest scoped observation for its service revision and execution | B1 observations, B3 routing |

A file capability is supported only for a qualified transport. The public upload ceiling is 67,108,864 bytes. The native framed download ceiling is 4 GiB. The legacy private single-frame GuestUpload ceiling is 524,288 bytes. Text preview and editing are client features constrained to 262,144 bytes of complete valid text. Files may have additional profile limits. A download interrupted after its response starts ends the response stream. An upload with an uncertain effect returns OUTCOME_UNKNOWN with effect unknown and retry verify_original_intent. Verify the original path/hash or reuse the same control epoch, idempotency key and body to retrieve a stored completion.

Metric zero is a valid measured number. Null represents a metric absent from a successful legacy runtime payload and an omitted measurement represents unavailable evidence. Check availability, issues and execution before using observations or calculating counter rates. System metrics sourced from OfficialRuntime cover that runtime only. A service observation of Worker upstream applies only to its scoped stage. A passive metrics or service read performs no Guest access or wake. Snapshot configuration provenance and active restore authorization have independent contracts.

Machine creation and native Fork target names require at least one non-whitespace character, at most 255 UTF-8 bytes and no NUL. The existing machine PATCH name accepts any string with a non-whitespace character. These are display name rules. The platform generates the runtime identifier.

## Client workflows

- TypeScript `getLifecycleStatus(machineID)` and Python `get_lifecycle_status(machine_id)` return the desired and applied policies and any available Worker observations. The CLI equivalent is `sandboxctl lifecycle status MACHINE`. Check the revisions before interpreting the effective policy.
- Use TypeScript `withActivity(machineID, reason, callback)` or Python `with_activity(machine_id, reason)` while waiting for an external response that should keep a machine running. A lease protects against automatic sleep. A manually requested Fork uses its own runtime capability and admission checks. The CLI supports `sandboxctl leases acquire MACHINE --semantics-version 2 --reason REASON` and `sandboxctl leases list MACHINE`.
- Observe the accepted operation ID with `waitOperation` / `wait_operation` or `sandboxctl operations show OPERATION`. A supported failed Fork target uses `retryForkTargets` / `retry_fork_targets` or `sandboxctl operations retry OPERATION` with the operation revision. It advances the existing logical child and attempt.
- For files, inspect machine `capabilities` (CLI `sandboxctl files capabilities MACHINE`), then list or stream with the SDK. For upload replay, preserve the intent returned by `client.intent()` before calling `uploadFile` / `upload_file`. A normal download returns raw bytes, and an interrupted stream raises an error when read.
