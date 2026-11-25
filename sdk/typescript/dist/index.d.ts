export type Effect = "none" | "applied" | "partial" | "unknown";
export type OperationState = "queued" | "dispatching" | "observing" | "succeeded" | "failed" | "cancelled" | "recovery_required";
export type PowerState = "running" | "standby" | "hibernated" | "stopped" | "unknown";
export type SourceDisposition = "RUNNING" | "STANDBY" | "HELD" | "EXITED" | "UNKNOWN";
export type CaptureMode = "vm" | "managed_tasks";
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
export interface ForkBatch {
    children: {
        name: string;
    }[];
    require_independent?: boolean;
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
    [key: string]: unknown;
}
export interface OperationChild {
    ordinal: number;
    machine_id: string;
    binding_id: string;
    state: string;
    resource_independence?: "unknown" | "verified" | "failed";
    error_code?: string;
}
export interface Operation {
    operation_id: string;
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
export interface Control {
    control_epoch: string;
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
export interface IdlePolicy {
    enabled: boolean;
    timeout_seconds: number;
}
export type BootMode = "oci" | "system";
export interface CreateMachine {
    boot_mode?: BootMode;
    runtime_instance_id?: string;
    name: string;
    template_id: string;
    resources?: Resources;
    idle_policy?: IdlePolicy;
    start?: boolean;
}
export interface Machine extends CreateMachine {
    machine_id: string;
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
    protocol: "http" | "https" | "ssh" | "tcp-mtls";
    guest_port: number;
    readiness?: unknown;
    auth?: unknown;
}
export interface Service extends ServiceSpec {
    service_id: string;
    machine_id: string;
    revision: number;
    public_host?: string;
    public_port?: number;
    enabled?: boolean;
    [key: string]: unknown;
}
export interface Snapshot {
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
    lifecycle?: "persistent" | "ephemeral";
}
export interface Volume extends VolumeSpec {
    volume_id: string;
    owner_id: string;
    runtime_bundle_id: string;
    lifecycle: "persistent" | "ephemeral";
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
    data: unknown;
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
    getOperation(id: string, signal?: AbortSignal): Promise<Operation>;
    getService(id: string, signal?: AbortSignal): Promise<Service>;
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
    restore(snapshotID: string, name: string, options?: WriteOptions): Promise<Operation>;
    listTokens(signal?: AbortSignal): AsyncGenerator<Token, any, any>;
    issueToken(spec: TokenSpec, options?: WriteOptions): Promise<Token>;
    revokeToken(id: string, revision: number, options?: WriteOptions): Promise<{
        token_id: string;
        revision: number;
        status: "revoked";
    }>;
    expose(id: string, spec: ServiceSpec, options?: WriteOptions): Promise<Operation>;
}
