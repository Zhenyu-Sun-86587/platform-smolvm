export type Effect = "none" | "applied" | "partial" | "unknown";
export type OperationState = "queued" | "dispatching" | "observing" | "succeeded" | "failed" | "cancelled" | "recovery_required";
export interface ErrorDetail {
    code?: string;
    message?: string;
    effect?: Effect;
    retry?: string;
    operation_id?: string;
    [key: string]: unknown;
}
export interface Operation {
    operation_id: string;
    state: OperationState;
    machine_id?: string;
    kind: string;
    stage: string;
    effect: Effect;
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
    storage_gib: number;
    overlay_gib: number;
}
export interface IdlePolicy {
    enabled: boolean;
    timeout_seconds: number;
}
export interface CreateMachine {
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
    power_state?: "running" | "standby" | "hibernated" | "stopped" | "unknown";
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
    machineAction(id: string, action: "start" | "stop" | "pause" | "resume" | "wake", generation: number, options: WriteOptions): Promise<Operation>;
    private machineMutation;
    startTask(id: string, spec: TaskSpec, options?: MachineWriteOptions): Promise<Operation>;
    getTask(id: string, signal?: AbortSignal): Promise<Task>;
    cancelTask(id: string, graceSeconds?: number, options?: WriteOptions): Promise<Operation>;
    capture(id: string, purpose: "clone" | "recovery", label: string, options?: MachineWriteOptions): Promise<Operation>;
    clone(id: string, childName: string, options?: MachineWriteOptions): Promise<Operation>;
    fork(id: string, childName: string, options?: MachineWriteOptions): Promise<Operation>;
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
