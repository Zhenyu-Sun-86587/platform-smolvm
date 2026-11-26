export type Effect = "none" | "applied" | "partial" | "unknown";
export type OperationState = "queued" | "dispatching" | "observing" | "succeeded" | "failed" | "cancelled" | "recovery_required";
export type PowerState = "running" | "standby" | "hibernated" | "stopped" | "unknown";
export type SourceDisposition = "RUNNING" | "STANDBY" | "HELD" | "EXITED" | "UNKNOWN";
export type CaptureMode = "vm" | "managed_tasks";
export type VolumeRestorePolicy = "exact" | "clone_conflicts";
export type BackupScope = "platform_snapshots" | "durable_runtime";
export interface BackupPolicy {
    policy_id: string;
    scope: BackupScope;
    interval_seconds: number;
    retain: number;
}
export interface BackupRun {
    backup_id: string;
    policy_id: string;
    scope: BackupScope;
    control_epoch: string;
    state: "running" | "verified" | "failed" | "retiring" | "retired";
    started_at: string;
    finished_at?: string;
    failure_code?: string;
}
export interface RuntimeProfile {
    runtime_instance_id: string;
    runtime_bundle_id: string;
    state_reader_id: string;
    status: string;
    verification_status: string;
    security_status: string;
    boot_profiles: string[];
    running_bindings: number;
    standby_bindings: number;
    hibernated_bindings: number;
    bundle_binding_refs: number;
    bundle_snapshot_refs: number;
    capabilities: Record<string, Capability>;
}
export interface ActivityPolicy {
    allow_active_tasks?: boolean;
}
export interface DesktopSession {
    desktop_session_id: string;
    machine_id: string;
    service_id: string;
    lease_id: string;
    binding_id: string;
    binding_generation: number;
    execution_epoch: number;
    state: "active" | "released" | "suspect";
    revision: number;
    expires_at: string;
}
export interface ForkBatch {
    children: {
        name: string;
    }[];
    require_independent?: boolean;
    admission?: "all_or_reject" | "best_effort";
    volume_policy?: {
        default?: "clone";
    };
}
export interface ErrorDetail {
    code?: string;
    message?: string;
    effect?: Effect;
    retry?: string;
    operation_id?: string;
    capability?: string;
    reason_code?: string;
    stage?: string;
    scope?: string;
    [key: string]: unknown;
}
export interface OperationChild {
    ordinal: number;
    machine_id: string;
    binding_id: string;
    state: string;
    attempt: number;
    resource_independence?: "unknown" | "verified" | "failed";
    error_code?: string;
}
export interface Operation {
    operation_id: string;
    control_epoch?: string;
    source_machine_id?: string;
    binding_id?: string;
    runtime_name?: string;
    created_at?: string;
    updated_at?: string;
    revision: number;
    state: OperationState;
    machine_id?: string;
    kind: string;
    stage: string;
    effect: Effect;
    source_disposition?: SourceDisposition;
    source_lock_released?: boolean;
    children?: OperationChild[];
    binding_generation?: number;
    error?: ErrorDetail | null;
    result?: Record<string, unknown> | null;
}
export interface Capability {
    status?: "supported" | "unsupported" | "unknown";
    contract_version?: number;
    constraints?: Record<string, unknown>;
    supported: boolean;
    reason_code?: string | null;
    message?: string;
    verified_profile?: string;
}
export type GuestFileCapability = "files.list" | "files.read" | "files.write" | "files.preview" | "files.edit";
export interface GuestFileConstraints {
    max_bytes?: number;
    transport?: "native_agent" | "official_runtime";
}
export interface GuestFileEntry {
    name: string;
    type: string;
    size: number;
}
export interface GuestFileUpload {
    upload_id: string;
    state: "uploaded";
    path: string;
    size: number;
    sha256: string;
}
export interface MetricExecution {
    binding_id: string;
    binding_generation: number;
    incarnation_id: string;
    execution_epoch: number;
}
export interface MetricObservation {
    availability?: "available" | "partial" | "unavailable" | "stale";
    source?: string;
    scope?: "machine" | "worker" | "official_runtime";
    observed_at?: string;
    execution?: MetricExecution;
    issues?: Record<string, {
        code: string;
        message: string;
    }>;
}
export interface PlatformMetrics {
    active_access_holds: number;
    wake_waiters: number;
    operations_by_state: Record<string, number>;
}
export interface PressureSample {
    someAvg10: number;
    someAvg60: number;
    fullAvg10: number;
    fullAvg60: number;
}
export interface MachineMetrics extends MetricObservation {
    name?: string;
    at?: string;
    state?: string;
    vcpus?: number;
    memory_limit_mb?: number;
    cpu_millis?: number | null;
    cpu_limit_millicpus?: number | null;
    memory_mb?: number | null;
    rss_mb?: number | null;
    disk_used_mb?: number | null;
    disk_gb?: number | null;
    egress_bytes?: number | null;
    pressure?: Record<"cpu" | "memory" | "io", PressureSample> | null;
    platform: PlatformMetrics;
}
export interface SystemMetrics extends MetricObservation {
    allocated_cpus?: number;
    allocated_memory_mb?: number;
    used_cpus?: number;
    used_memory_mb?: number;
    host_memory_total_mb?: number | null;
    host_memory_available_mb?: number | null;
    used_disk_gb?: number;
    boot_id?: string;
    platform: PlatformMetrics;
}
export interface Control {
    control_epoch: string;
    default_boot_mode?: BootMode;
    runtime_version?: string;
    build?: string;
    maintenance?: boolean;
    csrf_token?: string;
    capabilities?: Record<string, Capability>;
}
export interface Resources {
    cpu_count: number;
    memory_mib: number;
    storage_gib?: number;
    overlay_gib?: number;
}
export type ResourcePatch = Partial<Resources> & {
    cpu_limit_millicpus?: number | null;
    balloon_target_mib?: number;
};
export interface MachineResourceStatus {
    machine_id: string;
    binding_generation: number;
    power_state: string;
    requested: ResourcePatch;
    execution?: {
        instance_id: string;
        incarnation_id: string;
        execution_epoch: number;
    };
    compute?: {
        profile: string;
        capture_compatible: boolean;
        capabilities: Record<string, {
            supported: boolean;
            available_now: boolean;
            reason: string | null;
        }>;
        observed: {
            cpus?: {
                created: number;
                capacity: number;
            } | null;
            online_cpus?: number | null;
            memory_mib?: number | null;
            ram?: {
                boot_mib: number;
                mapped: number;
                plugged: number;
                capacity: number;
            } | null;
            balloon?: {
                target_mib: number;
                actual_mib: number;
            } | null;
            scope_memory_current_bytes?: number | null;
        };
        pending?: {
            operation_id: string;
            stage: string;
        } | null;
    };
    cpu_limit?: {
        requested_millicpus: number | null;
        quota_usec: number | null;
        period_usec: number;
    };
    issues: Record<string, {
        code: string;
        message: string;
    }>;
}
export interface IdlePolicy {
    enabled: boolean;
    timeout_seconds: number;
}
export interface WakeConditions {
    api_access: boolean;
    service_access: boolean;
    private_network_packet: boolean;
    allow_cold_start: boolean;
}
export interface LifecyclePolicy {
    schema_version: number;
    auto_standby: boolean;
    standby_after_seconds: number;
    auto_hibernate: boolean;
    hibernate_after_seconds: number;
    keep_running: boolean;
    reclaim_priority: number;
    pressure_reclaim: "disabled" | "idle_only";
    wake_conditions: WakeConditions;
    activity: {
        protect_running_tasks: boolean;
        protect_exec_sessions: boolean;
        protect_inbound_connections: boolean;
        protect_outbound_connections: boolean;
        protect_running_leases: true;
        unknown: "block_auto_sleep";
    };
    min_running_after_wake_seconds: number;
    retry_backoff_seconds: number;
}
export type LifecyclePolicyPatch = Partial<Omit<LifecyclePolicy, "wake_conditions" | "activity">> & {
    wake_conditions?: Partial<WakeConditions>;
    activity?: Partial<LifecyclePolicy["activity"]>;
};
export interface LifecycleWorkerStatus {
    power: string;
    blockers: string[];
    controlled_suspended: boolean;
    idle_ms?: number | null;
    next_action?: {
        kind: string;
        origin: string;
        eligible_after_ms: number;
    } | null;
    collectors: Record<string, {
        fresh?: boolean;
        session: string;
        seq: number;
    }>;
    last_decision?: {
        operation_id: string;
        kind: string;
        origin: string;
        stage: string;
        error?: string | null;
        start_worker_memory_bytes?: number | null;
        end_worker_memory_bytes?: number | null;
        created_at_ms?: number;
        finished_at_ms?: number | null;
    } | null;
    pressure?: {
        admission_throttled: boolean;
        sample?: Record<string, unknown> | null;
        last_balloon?: Record<string, unknown> | null;
    } | null;
    instance_id?: string;
    incarnation_id?: string | null;
    execution_epoch?: number;
    statepoint?: string | null;
    collector_generation?: string;
    activity_seq?: number;
    binding_revision?: number;
    applied_revision?: number;
    effective_policy?: LifecyclePolicy;
    first_launch_supported?: boolean;
}
export interface LifecycleStatus {
    supported: boolean;
    operation_id?: string;
    effective_status?: "pending" | "applied" | "disabled" | "blocked";
    effective_blockers?: string[];
    desired_revision: number;
    applied_revision: number;
    default_revision: number;
    source: string;
    desired: LifecyclePolicy;
    applied?: LifecyclePolicy;
    last_error?: string;
    overrides?: LifecyclePolicyPatch;
    worker?: LifecycleWorkerStatus;
}
export interface PolicyApplication {
    application_id: string;
    default_policy_revision: number;
    complete: boolean;
    policy: LifecyclePolicy;
    machines: {
        machine_id: string;
        expected_revision: number;
        state: "applied" | "pending" | "rejected";
        reason?: string;
    }[];
}
export interface RunningLease {
    lease_id: string;
    machine_id: string;
    semantics_version: number;
    execution_epoch?: number;
    binding_generation?: number;
    principal_id?: string;
    revision: number;
    state: string;
    reason: string;
    expires_at: string;
    release_reason?: string;
    lease_token?: string;
}
export type BootMode = "oci" | "system";
export type NetworkMode = "native" | "fixed_internal";
export interface NetworkSelection {
    network_id?: string;
    mode?: NetworkMode;
}
export interface PrivateNetwork {
    network_id: string;
    runtime_instance_id: string;
    cidr: string;
}
export interface MachineNetwork extends Required<NetworkSelection> {
    state: "published" | "reserved" | "unavailable";
    attachment_id?: string;
    private_ipv4?: string;
    guest_ipv4?: string;
    dns_name?: string;
}
export interface NetworkDefaults {
    default_network_mode?: NetworkMode;
    default_network_id?: string;
}
export interface CreateMachine {
    live_compute_growth?: boolean;
    lifecycle_policy?: LifecyclePolicyPatch;
    boot_mode?: BootMode;
    runtime_instance_id?: string;
    network?: NetworkSelection;
    name: string;
    template_id: string;
    resources?: Resources;
    idle_policy?: IdlePolicy;
    start?: boolean;
}
export interface Machine extends Omit<CreateMachine, "lifecycle_policy" | "network"> {
    lifecycle_policy?: LifecycleStatus;
    resources?: Resources & ResourcePatch;
    network?: Required<NetworkSelection>;
    machine_id: string;
    desktop_profile?: "software-vnc-v1";
    spec?: {
        resources?: Resources;
    };
    revision: number;
    binding_generation: number;
    power_state?: PowerState;
    health?: {
        vm: string;
        guest_management: string;
        workload: string;
        workload_error?: {
            code: string;
            message: string;
        };
        network_identity: string;
        services: string;
    };
    observed_state: string;
    availability: string;
    desired_power: string;
    operation_id?: string | null;
    observed_at?: string | null;
    last_user_activity_at?: string | null;
    blocked_reasons?: string[];
    capabilities?: Record<string, Capability>;
    runtime_identity?: Record<string, unknown> | null;
    runtime_name?: string;
    guest_management_status?: string;
    task_metadata_status?: "complete" | "unavailable" | "stale";
    state_reason?: unknown;
    binding?: {
        binding_id?: string;
        runtime_instance_id?: string;
        runtime_bundle_id?: string;
        execution_epoch?: number;
        incarnation_id?: string;
        runtime_identity?: Record<string, unknown> | null;
        state_reason?: unknown;
        guest_management_evidence?: {
            ssh_ready?: boolean;
            ssh_host_key?: string;
        };
    };
    keep_running?: boolean;
}
export interface TaskSpec {
    argv: string[];
    cwd?: string;
    user?: string;
    env?: Record<string, string>;
    task_key?: string;
    output_limit?: number;
    output_policy?: string;
}
export interface Task {
    task_id?: string;
    id?: string;
    machine_id: string;
    state: "accepted" | "starting" | "running" | "cancelling" | "succeeded" | "failed" | "cancelled" | "unknown" | "interrupted";
    observed_at?: string;
    exit_code?: number | null;
    origin_machine_id?: string;
    origin_task_id?: string;
    revision?: number;
    operation_id?: string;
}
export interface ServiceSpec {
    name: string;
    protocol: "http" | "https" | "ssh" | "tcp-mtls" | "vnc";
    guest_port: number;
    readiness?: unknown;
    auth?: unknown;
}
export interface ServiceObservation {
    status: "unknown" | "ready" | "unavailable" | "stale";
    stage: "configuration" | "wake" | "guest_management" | "worker_upstream" | "public_dns" | "public_tls";
    scope: "configuration" | "worker_upstream" | "public_ingress";
    service_revision: number;
    binding_generation?: number;
    execution_epoch?: number;
    observed_at?: string;
    reason_code?: string;
    operation_id?: string;
}
export interface Service extends ServiceSpec {
    observation?: ServiceObservation;
    service_id: string;
    machine_id: string;
    revision: number;
    public_host?: string;
    public_port?: number;
    enabled?: boolean;
    [key: string]: unknown;
}
export interface CaptureConfiguration {
    status: "verified" | "unknown";
    schema_version?: number;
    source_machine_revision?: number;
    capture_id?: string;
    manifest_digest?: string;
    template_id?: string;
    source?: string;
    spec_digest?: string;
    spec?: {
        template_id?: string;
        boot_mode?: BootMode;
        resources?: ResourcePatch;
        live_compute_growth?: boolean;
        network?: NetworkSelection & {
            route_domain_id?: string;
            cidr?: string;
        };
    };
    reason_code?: string;
}
export interface CaptureSummary {
    capture_mode: CaptureMode;
    configuration: CaptureConfiguration;
    ram: "included" | "excluded" | "unknown";
    ram_bytes?: number;
    disks?: {
        role: string;
        device_id?: string;
        volume_id?: string;
        generation_id?: string;
        read_only?: boolean;
        capacity_bytes?: number;
    }[];
    verified_at?: string;
    restore_policies?: VolumeRestorePolicy[];
    reason_code?: string;
}
export interface Snapshot {
    capture_summary?: CaptureSummary;
    snapshot_id: string;
    capture_mode?: CaptureMode;
    vm_provenance?: {
        operation_id: string;
        runtime_instance_id: string;
        runtime_bundle_id: string;
        state_reader_id: string;
        runtime_capture_id: string;
    };
    source_machine_id?: string;
    revision?: number;
    purpose: "clone" | "recovery";
    task_content: string;
    state: string;
    label?: string;
    sha256?: string;
    size_bytes?: number;
    created_at?: string;
    [key: string]: unknown;
}
export interface TokenSpec {
    label: string;
    scope: {
        access: "read" | "manage" | "tunnel";
        machine_id?: string;
    };
    expires_at?: string;
}
export interface VolumeSpec {
    name: string;
    capacity_bytes: number;
    runtime_instance_id: string;
    lifecycle?: "persistent" | "ephemeral" | "machine";
}
export interface Volume extends VolumeSpec {
    volume_id: string;
    owner_id: string;
    runtime_bundle_id: string;
    lifecycle: "persistent" | "ephemeral" | "machine";
    machine_instance_id?: string;
    revision: number;
    state: "available" | "changing" | "deleted";
    head_resource_id?: string;
    generation_id?: string;
    snapshots: VolumeSnapshot[];
    attachments: VolumeAttachment[];
}
export interface VolumeSnapshot {
    snapshot_id: string;
    generation_id: string;
    source_revision: number;
    capacity_bytes: number;
    volume_id?: string;
    volume_revision?: number;
}
export interface VolumeAttachmentSpec {
    volume_id: string;
    device_key: string;
    mode?: "rw" | "ro";
    snapshot_id?: string;
    fork_policy?: "clone" | "share_immutable" | "reject";
    capture_policy?: "include" | "external";
    expected_volume_revision?: number;
}
export interface VolumeAttachment extends Pick<VolumeAttachmentSpec, "device_key"> {
    attachment_id: string;
    instance_id: string;
    mode: "rw" | "ro";
    fork_policy: "clone" | "share_immutable" | "reject";
    capture_policy: "include" | "external";
    state: "reserved" | "attached" | "unknown";
    revision: number;
    generation_id?: string;
    volume_id?: string;
    machine_id?: string;
    binding_id?: string;
}
export interface Token extends TokenSpec {
    token_id: string;
    revision: number;
    created_at: string;
    revoked_at?: string;
    token?: string;
}
export interface Mutation {
    epoch: string;
    key: string;
}
export interface WriteOptions {
    method?: string;
    revision?: number;
    intent?: Mutation;
    signal?: AbortSignal;
}
export interface MachineWriteOptions extends WriteOptions {
    generation?: number;
}
export interface PrivatePacketWakeRule {
    rule_id: string;
    source_attachment_id: string;
    target_attachment_id: string;
    transport: "tcp" | "udp";
    guest_port: number;
    enabled: boolean;
    revision: number;
    source_current: boolean;
    source_incarnation_id?: string | null;
    source_execution_epoch?: number | null;
    last_wake?: {
        operation_id: string;
        rule_revision: number;
        execution_epoch: number;
        authorized: boolean;
        datagram?: "forwarded" | "dropped" | "unknown" | null;
    } | null;
}
export interface Page<T> {
    items: T[];
    next_cursor?: string;
}
export interface TaskChunk {
    start: number;
    next: number;
    gap: boolean;
    bytes: Uint8Array;
}
export interface PlatformEvent {
    id: string;
    type: string;
    data: {
        id: string;
        control_epoch: string;
        seq: number;
        type: string;
        created_at: string;
        payload: unknown;
        machine_id?: string;
        operation_id?: string;
        event_name?: string;
        resource_kind?: string;
        resource_id?: string;
        schema_version?: number;
    };
}
export interface StreamTicket {
    ticket: string;
    protocol?: string;
}
export declare const terminalStates: Set<OperationState>;
export declare class PlatformError extends Error {
    readonly status: number;
    readonly detail: ErrorDetail;
    constructor(status: number, detail: ErrorDetail);
}
export declare function machine(id: string): string;
export declare function task(id: string): string;
export declare function snapshot(id: string): string;
export declare function volume(id: string): string;
export declare function volumeSnapshot(id: string): string;
export declare function service(id: string): string;
export declare function operation(id: string): string;
export declare class Client {
    private readonly token?;
    private readonly base;
    private csrf?;
    constructor(url: string, token?: string | undefined);
    private url;
    response(path: string, init?: RequestInit): Promise<Response>;
    private json;
    get<T>(path: string, signal?: AbortSignal): Promise<T>;
    control(signal?: AbortSignal): Promise<Control>;
    login(username: string, password: string): Promise<{
        csrf_token: string;
    }>;
    logout(): Promise<void>;
    intent(signal?: AbortSignal): Promise<Mutation>;
    private writeHeaders;
    mutate<T = Operation>(path: string, body?: unknown, options?: WriteOptions): Promise<T>;
    upload<T = Operation>(path: string, blob: Blob, options?: WriteOptions & {
        sha256?: string;
    }): Promise<T>;
    waitOperation(id: string, { timeoutMs, signal, onUpdate }?: {
        timeoutMs?: number;
        signal?: AbortSignal;
        onUpdate?: (op: Operation) => void;
    }): Promise<Operation>;
    wait(id: string, timeoutMs?: number): Promise<Operation>;
    pages<T>(path: string, limit?: number, signal?: AbortSignal): AsyncGenerator<T>;
    taskOutput(id: string, stream?: string, offset?: number, signal?: AbortSignal): AsyncGenerator<TaskChunk>;
    events(lastEventId?: string, signal?: AbortSignal): AsyncGenerator<PlatformEvent>;
    browserSocket(path: string, ticketPath: string, protocol: string, signal?: AbortSignal): Promise<WebSocket>;
    socket<T>(path: string, protocol: string, connect: (url: URL, protocol: string, headers: Headers) => T): T;
    createMachine(spec: CreateMachine, options?: WriteOptions): Promise<Operation>;
    getMachine(id: string, signal?: AbortSignal): Promise<Machine>;
    getMachineMetrics(id: string, signal?: AbortSignal): Promise<MachineMetrics>;
    getSystemMetrics(signal?: AbortSignal): Promise<SystemMetrics>;
    listFiles(id: string, path?: string, signal?: AbortSignal): AsyncGenerator<GuestFileEntry, any, any>;
    downloadFile(id: string, path: string, signal?: AbortSignal): Promise<Response>;
    uploadFile(id: string, path: string, data: Blob, options?: Omit<WriteOptions, "method">): Promise<GuestFileUpload>;
    getMachineResources(id: string, signal?: AbortSignal): Promise<MachineResourceStatus>;
    configureResources(id: string, resources: ResourcePatch, options?: MachineWriteOptions): Promise<Operation>;
    listPrivateNetworks(signal?: AbortSignal): AsyncGenerator<PrivateNetwork, any, any>;
    getMachineNetwork(id: string, signal?: AbortSignal): Promise<MachineNetwork>;
    setNetworkDefaults(defaults: NetworkDefaults, options?: WriteOptions): Promise<NetworkDefaults & {
        revision: number;
    }>;
    getLifecycleStatus(id: string, signal?: AbortSignal): Promise<LifecycleStatus>;
    setDefaultLifecyclePolicy(policy: LifecyclePolicyPatch, options?: WriteOptions): Promise<{
        revision: number;
        default_lifecycle_revision: number;
        default_lifecycle_policy: LifecyclePolicy;
    }>;
    updateLifecyclePolicy(id: string, policy: LifecyclePolicyPatch, options?: MachineWriteOptions & {
        policyRevision?: number;
    }): Promise<Operation>;
    applyDefaultLifecyclePolicy(revision: number, machines: {
        machine_id: string;
        expected_revision: number;
    }[], options?: WriteOptions): Promise<PolicyApplication>;
    listRunningLeases(id: string, signal?: AbortSignal): AsyncGenerator<RunningLease, any, any>;
    acquireRunningLease(id: string, reason: string, ttlSeconds: number, options?: WriteOptions): Promise<RunningLease>;
    renewRunningLease(lease: RunningLease, ttlSeconds: number, options?: WriteOptions): Promise<{
        lease_token: string;
        lease_id: string;
        machine_id: string;
        semantics_version: number;
        execution_epoch?: number;
        binding_generation?: number;
        principal_id?: string;
        revision: number;
        state: string;
        reason: string;
        expires_at: string;
        release_reason?: string;
    }>;
    releaseRunningLease(lease: RunningLease, options?: WriteOptions): Promise<RunningLease>;
    withActivity<T>(id: string, reason: string, run: (signal: AbortSignal) => Promise<T>, ttlSeconds?: number): Promise<T>;
    getPrivatePacketWakeRule(id: string, source: string, transport: "tcp" | "udp", port: number, signal?: AbortSignal): Promise<PrivatePacketWakeRule>;
    setPrivatePacketWakeRule(id: string, source: string, transport: "tcp" | "udp", port: number, enabled: boolean, ruleRevision: number, options?: MachineWriteOptions): Promise<PrivatePacketWakeRule>;
    getOperation(id: string, signal?: AbortSignal): Promise<Operation>;
    listBackupPolicies(signal?: AbortSignal): AsyncGenerator<BackupPolicy, any, any>;
    listBackups(signal?: AbortSignal): AsyncGenerator<BackupRun, any, any>;
    getBackup(id: string, signal?: AbortSignal): Promise<BackupRun>;
    runBackup(policy: string, options?: WriteOptions): Promise<BackupRun>;
    retryForkTargets(id: string, options?: WriteOptions): Promise<Operation>;
    cancelOperation(id: string, options?: WriteOptions): Promise<Operation>;
    listServices(id: string, signal?: AbortSignal): AsyncGenerator<Service, any, any>;
    getService(id: string, signal?: AbortSignal): Promise<Service>;
    listDesktopSessions(id: string, signal?: AbortSignal): AsyncGenerator<DesktopSession, any, any>;
    createDesktopSession(id: string, serviceId: string, ttlSeconds?: number, options?: MachineWriteOptions): Promise<DesktopSession>;
    closeDesktopSession(id: string, options?: WriteOptions): Promise<DesktopSession>;
    desktopSocket(id: string, signal?: AbortSignal): Promise<WebSocket>;
    listSnapshots(id?: string, signal?: AbortSignal): AsyncGenerator<Snapshot, any, any>;
    getSnapshot(id: string, signal?: AbortSignal): Promise<Snapshot>;
    listMachines(signal?: AbortSignal): AsyncGenerator<Machine, any, any>;
    listVolumes(signal?: AbortSignal): AsyncGenerator<Volume, any, any>;
    createVolume(spec: VolumeSpec, options?: WriteOptions): Promise<Operation>;
    getVolume(id: string, signal?: AbortSignal): Promise<Volume>;
    growVolume(id: string, capacityBytes: number, options?: WriteOptions): Promise<Operation>;
    deleteVolume(id: string, options?: WriteOptions): Promise<Operation>;
    listVolumeSnapshots(id: string, signal?: AbortSignal): AsyncGenerator<VolumeSnapshot, any, any>;
    getVolumeSnapshot(id: string, signal?: AbortSignal): Promise<VolumeSnapshot & {
        volume_id: string;
        volume_revision: number;
    }>;
    createVolumeSnapshot(id: string, options?: WriteOptions): Promise<Operation>;
    deleteVolumeSnapshot(id: string, options?: WriteOptions): Promise<Operation>;
    cloneVolumeSnapshot(id: string, name: string, options?: WriteOptions): Promise<Operation>;
    listVolumeAttachments(id: string, signal?: AbortSignal): AsyncGenerator<VolumeAttachment & {
        volume_id: string;
        machine_id: string;
        binding_id: string;
    }, any, any>;
    attachVolume(id: string, spec: VolumeAttachmentSpec, options?: MachineWriteOptions): Promise<Operation>;
    detachVolume(id: string, volumeRevision: number, options: MachineWriteOptions & {
        generation: number;
        revision: number;
    }): Promise<Operation>;
    listRuntimeProfiles(signal?: AbortSignal): AsyncGenerator<RuntimeProfile, any, any>;
    machineAction(id: string, action: "start" | "stop" | "pause" | "resume" | "wake", generation: number, options: WriteOptions): Promise<Operation>;
    private machineMutation;
    standby(id: string, activityPolicy?: ActivityPolicy, options?: MachineWriteOptions): Promise<Operation>;
    hibernate(id: string, activityPolicy?: ActivityPolicy, options?: MachineWriteOptions): Promise<Operation>;
    startTask(id: string, spec: TaskSpec, options?: MachineWriteOptions): Promise<Operation>;
    getTask(id: string, signal?: AbortSignal): Promise<Task>;
    cancelTask(id: string, graceSeconds?: number, options?: WriteOptions): Promise<Operation>;
    capture(id: string, purpose: "clone" | "recovery", label: string, { captureMode, ...options }?: MachineWriteOptions & {
        captureMode?: CaptureMode;
    }): Promise<Operation>;
    checkpoint(id: string, label?: string, options?: MachineWriteOptions): Promise<Operation>;
    clone(id: string, childName: string, options?: MachineWriteOptions): Promise<Operation>;
    fork(id: string, childName: string, { requireIndependent, ...options }?: MachineWriteOptions & {
        requireIndependent?: boolean;
    }): Promise<Operation>;
    forkBatch(id: string, request: ForkBatch, options?: MachineWriteOptions): Promise<Operation>;
    restore(snapshotID: string, name: string, { volumeRestorePolicy, ...options }?: WriteOptions & {
        volumeRestorePolicy?: VolumeRestorePolicy;
    }): Promise<Operation>;
    rollbackVM(id: string, snapshotID: string, volumeRestorePolicy?: VolumeRestorePolicy, options?: MachineWriteOptions): Promise<Operation>;
    listTokens(signal?: AbortSignal): AsyncGenerator<Token, any, any>;
    issueToken(spec: TokenSpec, options?: WriteOptions): Promise<Token>;
    revokeToken(id: string, revision: number, options?: WriteOptions): Promise<{
        token_id: string;
        revision: number;
        status: "revoked";
    }>;
    expose(id: string, spec: ServiceSpec, options?: WriteOptions): Promise<Operation>;
}
