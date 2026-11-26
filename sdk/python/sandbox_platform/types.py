"""Public wire views returned as dictionaries by the client."""

from typing import Literal, TypedDict

Effect = Literal["none", "applied", "partial", "unknown"]
OperationState = Literal["queued", "dispatching", "observing", "succeeded", "failed", "cancelled", "recovery_required"]
PowerState = Literal["running", "standby", "hibernated", "stopped", "unknown"]


class Capability(TypedDict, total=False):
    supported: bool
    status: Literal["supported", "unsupported", "unknown"]
    constraints: dict[str, object]
    contract_version: int
    reason_code: str | None
    message: str
    verified_profile: str


class OperationChild(TypedDict, total=False):
    ordinal: int
    machine_id: str
    binding_id: str
    state: str
    attempt: int
    resource_independence: Literal["unknown", "verified", "failed"]
    error_code: str


class Operation(TypedDict, total=False):
    operation_id: str
    control_epoch: str
    machine_id: str
    source_machine_id: str
    revision: int
    binding_id: str
    binding_generation: int
    runtime_name: str
    state: OperationState
    kind: str
    stage: str
    effect: Effect
    source_disposition: Literal["RUNNING", "STANDBY", "HELD", "EXITED", "UNKNOWN"]
    source_lock_released: bool
    children: list[OperationChild]
    result: dict[str, object] | None
    error: dict[str, object] | None
    created_at: str
    updated_at: str


class LifecyclePolicy(TypedDict, total=False):
    schema_version: int
    auto_standby: bool
    standby_after_seconds: int
    auto_hibernate: bool
    hibernate_after_seconds: int
    keep_running: bool
    reclaim_priority: int
    pressure_reclaim: Literal["disabled", "idle_only"]
    wake_conditions: dict[str, bool]
    activity: dict[str, bool | Literal["block_auto_sleep"]]
    min_running_after_wake_seconds: int
    retry_backoff_seconds: int


class LifecycleCollector(TypedDict, total=False):
    fresh: bool
    session: str
    seq: int


class LifecycleWorkerStatus(TypedDict, total=False):
    power: str
    blockers: list[str]
    controlled_suspended: bool
    idle_ms: int | None
    next_action: dict[str, object] | None
    collectors: dict[str, LifecycleCollector]
    last_decision: dict[str, object] | None
    pressure: dict[str, object] | None
    instance_id: str
    incarnation_id: str | None
    execution_epoch: int
    statepoint: str | None
    collector_generation: str
    activity_seq: int
    binding_revision: int
    applied_revision: int
    effective_policy: LifecyclePolicy
    first_launch_supported: bool


class LifecycleStatus(TypedDict, total=False):
    supported: bool
    desired_revision: int
    applied_revision: int
    default_revision: int
    source: str
    desired: LifecyclePolicy
    applied: LifecyclePolicy
    overrides: LifecyclePolicy
    operation_id: str
    last_error: str
    effective_status: Literal["pending", "applied", "disabled", "blocked"]
    effective_blockers: list[str]
    worker: LifecycleWorkerStatus


class Machine(TypedDict, total=False):
    machine_id: str
    name: str
    template_id: str
    revision: int
    binding_generation: int
    power_state: PowerState
    observed_state: str
    availability: str
    desired_power: str
    health: dict[str, object]
    boot_mode: Literal["oci", "system"]
    desktop_profile: str
    resources: dict[str, int | None]
    spec: dict[str, object]
    network: dict[str, str]
    runtime_name: str
    runtime_identity: dict[str, object] | None
    operation_id: str
    observed_at: str
    last_user_activity_at: str
    guest_management_status: str
    task_metadata_status: str
    blocked_reasons: list[str]
    keep_running: bool
    binding: dict[str, object]
    capabilities: dict[str, Capability]
    lifecycle_policy: LifecycleStatus


class RunningLease(TypedDict, total=False):
    lease_id: str
    machine_id: str
    principal_id: str
    binding_generation: int
    semantics_version: int
    execution_epoch: int
    state: str
    revision: int
    reason: str
    expires_at: str
    release_reason: str
    lease_token: str


class GuestFileEntry(TypedDict):
    name: str
    type: str
    size: int


class GuestFileUpload(TypedDict):
    upload_id: str
    state: Literal["uploaded"]
    path: str
    size: int
    sha256: str


class MetricIssue(TypedDict):
    code: str
    message: str


class MetricExecution(TypedDict):
    binding_id: str
    binding_generation: int
    incarnation_id: str
    execution_epoch: int


class PlatformMetrics(TypedDict):
    active_access_holds: int
    wake_waiters: int
    operations_by_state: dict[str, int]


class MetricObservation(TypedDict, total=False):
    availability: Literal["available", "partial", "unavailable", "stale"]
    source: str
    scope: Literal["machine", "worker", "official_runtime"]
    observed_at: str
    execution: MetricExecution
    issues: dict[str, MetricIssue]
    platform: PlatformMetrics


class MachineMetrics(MetricObservation, total=False):
    name: str
    at: str
    state: str
    vcpus: int
    memory_limit_mb: int
    cpu_millis: int | None
    cpu_limit_millicpus: int | None
    memory_mb: int | None
    rss_mb: int | None
    disk_used_mb: int | None
    disk_gb: int | None
    egress_bytes: int | None
    pressure: dict[str, dict[str, float]] | None


class SystemMetrics(MetricObservation, total=False):
    allocated_cpus: int
    allocated_memory_mb: int
    used_cpus: float
    used_memory_mb: int
    host_memory_total_mb: int | None
    host_memory_available_mb: int | None
    used_disk_gb: int
    boot_id: str


class ServiceObservation(TypedDict, total=False):
    status: Literal["unknown", "ready", "unavailable", "stale"]
    stage: Literal["configuration", "wake", "guest_management", "worker_upstream", "public_dns", "public_tls"]
    scope: Literal["configuration", "worker_upstream", "public_ingress"]
    service_revision: int
    binding_generation: int
    execution_epoch: int
    observed_at: str
    reason_code: str
    operation_id: str


class Service(TypedDict, total=False):
    service_id: str
    machine_id: str
    revision: int
    name: str
    protocol: str
    guest_port: int
    public_host: str
    public_port: int
    enabled: bool
    readiness: dict[str, object]
    auth: dict[str, object]
    ssh_host_key: str
    observation: ServiceObservation


class CaptureConfiguration(TypedDict, total=False):
    status: Literal["verified", "unknown"]
    source: str
    schema_version: int
    source_machine_revision: int
    capture_id: str
    manifest_digest: str
    spec_digest: str
    template_id: str
    spec: dict[str, object]
    reason_code: str


class CaptureSummary(TypedDict, total=False):
    capture_mode: Literal["vm", "managed_tasks"]
    configuration: CaptureConfiguration
    ram: Literal["included", "excluded", "unknown"]
    ram_bytes: int
    disks: list[dict[str, object]]
    restore_policies: list[Literal["exact", "clone_conflicts"]]
    verified_at: str
    reason_code: str


class Snapshot(TypedDict, total=False):
    snapshot_id: str
    source_machine_id: str
    revision: int
    purpose: Literal["clone", "recovery"]
    task_content: Literal["clean", "active", "unknown"]
    state: str
    label: str
    size_bytes: int
    sha256: str
    created_at: str
    manifest: dict[str, object]
    provenance: str
    closure_verified_at: str
    restore_verified_at: str
    capture_mode: Literal["vm", "managed_tasks"]
    capture_summary: CaptureSummary
    vm_provenance: dict[str, str]
