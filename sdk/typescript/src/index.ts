export type Effect = "none" | "applied" | "partial" | "unknown"
export type OperationState = "queued" | "dispatching" | "observing" | "succeeded" | "failed" | "cancelled" | "recovery_required"
export type PowerState = "running" | "standby" | "hibernated" | "stopped" | "unknown"
export type SourceDisposition = "RUNNING" | "STANDBY" | "HELD" | "EXITED" | "UNKNOWN"
export type CaptureMode = "vm" | "managed_tasks"
export type VolumeRestorePolicy = "exact" | "clone_conflicts"
export type BackupScope = "platform_snapshots" | "durable_runtime"
export interface BackupPolicy { policy_id: string; scope: BackupScope; interval_seconds: number; retain: number }
export interface BackupRun {
  backup_id: string; policy_id: string; scope: BackupScope; control_epoch: string
  state: "running" | "verified" | "failed" | "retiring" | "retired"
  started_at: string; finished_at?: string; failure_code?: string
}
export interface RuntimeProfile {
  runtime_instance_id: string
  runtime_bundle_id: string
  state_reader_id: string
  status: string
  verification_status: string
  security_status: string
  boot_profiles: string[]
  running_bindings: number
  standby_bindings: number
  hibernated_bindings: number
  bundle_binding_refs: number
  bundle_snapshot_refs: number
  capabilities: Record<string, Capability>
}
export interface ActivityPolicy { allow_active_tasks?: boolean }
export interface DesktopSession {
  desktop_session_id: string
  machine_id: string
  service_id: string
  lease_id: string
  binding_id: string
  binding_generation: number
  execution_epoch: number
  state: "active" | "released" | "suspect"
  revision: number
  expires_at: string
}
export interface ForkBatch {
  children: { name: string }[]
  require_independent?: boolean
  admission?: "all_or_reject" | "best_effort"
  volume_policy?: { default?: "clone" }
}
export interface ErrorDetail {
  code?: string
  message?: string
  effect?: Effect
  retry?: string
  operation_id?: string
  capability?: string
  reason_code?: string
  [key: string]: unknown
}
export interface OperationChild {
  ordinal: number
  machine_id: string
  binding_id: string
  state: string
  attempt: number
  resource_independence?: "unknown" | "verified" | "failed"
  error_code?: string
}
export interface Operation {
  operation_id: string
  revision: number
  state: OperationState
  machine_id?: string
  kind: string
  stage: string
  effect: Effect
  source_disposition?: SourceDisposition
  source_lock_released?: boolean
  children?: OperationChild[]
  binding_generation?: number
  error?: ErrorDetail | null
  result?: Record<string, unknown> | null
}
export interface Capability { status?: "supported" | "unsupported" | "unknown"; contract_version?: number; constraints?: Record<string, unknown>; supported: boolean; reason_code?: string | null; message?: string; verified_profile?: string }
export interface Control {
  control_epoch: string
  default_boot_mode?: BootMode
  runtime_version?: string
  build?: string
  maintenance?: boolean
  csrf_token?: string
  capabilities?: Record<string, Capability>
}
export interface Resources { cpu_count: number; memory_mib: number; storage_gib?: number; overlay_gib?: number }
export type ResourcePatch = Partial<Resources> & { cpu_limit_millicpus?: number | null; balloon_target_mib?: number }
export interface MachineResourceStatus {
  machine_id: string
  binding_generation: number
  power_state: string
  requested: ResourcePatch
  execution?: { instance_id: string; incarnation_id: string; execution_epoch: number }
  compute?: {
    profile: string
    capture_compatible: boolean
    capabilities: Record<string, { supported: boolean; available_now: boolean; reason: string | null }>
    observed: {
      cpus?: { created: number; capacity: number } | null; online_cpus?: number | null
      memory_mib?: number | null
      ram?: { boot_mib: number; mapped: number; plugged: number; capacity: number } | null
      balloon?: { target_mib: number; actual_mib: number } | null; scope_memory_current_bytes?: number | null
    }
    pending?: { operation_id: string; stage: string } | null
  }
  cpu_limit?: { requested_millicpus: number | null; quota_usec: number | null; period_usec: number }
  issues: Record<string, { code: string; message: string }>
}
export interface IdlePolicy { enabled: boolean; timeout_seconds: number }
export interface WakeConditions { api_access: boolean; service_access: boolean; private_network_packet: boolean; allow_cold_start: boolean }
export interface LifecyclePolicy {
  schema_version: number; auto_standby: boolean; standby_after_seconds: number
  auto_hibernate: boolean; hibernate_after_seconds: number; keep_running: boolean
  reclaim_priority: number; pressure_reclaim: "disabled" | "idle_only"; wake_conditions: WakeConditions
  activity: { protect_running_tasks: boolean; protect_exec_sessions: boolean; protect_inbound_connections: boolean;
    protect_outbound_connections: boolean; protect_running_leases: true; unknown: "block_auto_sleep" }
  min_running_after_wake_seconds: number; retry_backoff_seconds: number
}
export type LifecyclePolicyPatch = Partial<Omit<LifecyclePolicy, "wake_conditions" | "activity">> & {
  wake_conditions?: Partial<WakeConditions>; activity?: Partial<LifecyclePolicy["activity"]>
}
export interface LifecycleStatus {
  supported: boolean
  effective_status?: "pending" | "applied" | "disabled" | "blocked"
  effective_blockers?: string[]
  desired_revision: number; applied_revision: number; default_revision: number; source: string
  desired: LifecyclePolicy; applied?: LifecyclePolicy; last_error?: string; overrides?: LifecyclePolicyPatch
  worker?: { power: string; blockers: string[]; idle_ms?: number; controlled_suspended: boolean;
    next_action?: { kind: string; origin: string; eligible_after_ms: number } | null;
    last_decision?: { operation_id: string; kind: string; stage: string }; collectors: Record<string, unknown> }
}
export interface PolicyApplication {
  application_id: string; default_policy_revision: number; complete: boolean; policy: LifecyclePolicy
  machines: { machine_id: string; expected_revision: number; state: "applied" | "pending" | "rejected"; reason?: string }[]
}
export interface RunningLease {
  lease_id: string; machine_id: string; semantics_version: number; execution_epoch?: number
  revision: number; state: string; reason: string; expires_at: string; release_reason?: string; lease_token?: string
}
export type BootMode = "oci" | "system"
export type NetworkMode = "native" | "fixed_internal"
export interface NetworkSelection { network_id?: string; mode?: NetworkMode }
export interface PrivateNetwork { network_id: string; runtime_instance_id: string; cidr: string }
export interface MachineNetwork extends Required<NetworkSelection> {
  state: "published" | "reserved" | "unavailable"
  attachment_id?: string
  private_ipv4?: string
  guest_ipv4?: string
  dns_name?: string
}
export interface NetworkDefaults { default_network_mode?: NetworkMode; default_network_id?: string }
export interface CreateMachine {
  live_compute_growth?: boolean
  lifecycle_policy?: LifecyclePolicyPatch
  boot_mode?: BootMode
  runtime_instance_id?: string
  network?: NetworkSelection
  name: string
  template_id: string
  resources?: Resources
  idle_policy?: IdlePolicy
  start?: boolean
}
export interface Machine extends Omit<CreateMachine, "lifecycle_policy" | "network"> {
  lifecycle_policy?: LifecycleStatus
  resources?: Resources & ResourcePatch
  network?: Required<NetworkSelection>
  machine_id: string
  desktop_profile?: "software-vnc-v1"
  spec?: { resources?: Resources }
  revision: number
  binding_generation: number
  power_state?: PowerState
  health?: {vm: string; guest_management: string; workload: string; workload_error?: {code: string; message: string}; network_identity: string; services: string}
  observed_state: string
  availability: string
  desired_power: string
  operation_id?: string | null
  observed_at?: string | null
  last_user_activity_at?: string | null
  blocked_reasons?: string[]
  capabilities?: Record<string, Capability>
  runtime_identity?: Record<string, unknown> | null
  runtime_name?: string
  guest_management_status?: string
  task_metadata_status?: "complete" | "unavailable" | "stale"
  state_reason?: unknown
  binding?: {
    binding_id?: string
    runtime_instance_id?: string
    runtime_bundle_id?: string
    execution_epoch?: number
    incarnation_id?: string
    runtime_identity?: Record<string, unknown> | null
    state_reason?: unknown
    guest_management_evidence?: { ssh_ready?: boolean; ssh_host_key?: string }
  }
  keep_running?: boolean
}
export interface TaskSpec {
  argv: string[]
  cwd?: string
  user?: string
  env?: Record<string, string>
  task_key?: string
  output_limit?: number
  output_policy?: string
}
export interface Task {
  task_id?: string
  id?: string
  machine_id: string
  state: "accepted" | "starting" | "running" | "cancelling" | "succeeded" | "failed" | "cancelled" | "unknown" | "interrupted"
  observed_at?: string
  exit_code?: number | null
  origin_machine_id?: string
  origin_task_id?: string
  revision?: number
  operation_id?: string
}
export interface ServiceSpec {
  name: string
  protocol: "http" | "https" | "ssh" | "tcp-mtls" | "vnc"
  guest_port: number
  readiness?: unknown
  auth?: unknown
}
export interface Service extends ServiceSpec {
  service_id: string
  machine_id: string
  revision: number
  public_host?: string
  public_port?: number
  enabled?: boolean
  [key: string]: unknown
}
export interface Snapshot {
  snapshot_id: string
  capture_mode?: CaptureMode
  vm_provenance?: {
    operation_id: string
    runtime_instance_id: string
    runtime_bundle_id: string
    state_reader_id: string
    runtime_capture_id: string
  }
  source_machine_id?: string
  revision?: number
  purpose: "clone" | "recovery"
  task_content: string
  state: string
  label?: string
  sha256?: string
  size_bytes?: number
  created_at?: string
  [key: string]: unknown
}
export interface TokenSpec {
  label: string
  scope: { access: "read" | "manage" | "tunnel"; machine_id?: string }
  expires_at?: string
}
export interface VolumeSpec {
  name: string
  capacity_bytes: number
  runtime_instance_id: string
  lifecycle?: "persistent" | "ephemeral" | "machine"
}
export interface Volume extends VolumeSpec {
  volume_id: string
  owner_id: string
  runtime_bundle_id: string
  lifecycle: "persistent" | "ephemeral" | "machine"
  machine_instance_id?: string
  revision: number
  state: "available" | "changing" | "deleted"
  head_resource_id?: string
  generation_id?: string
  snapshots: VolumeSnapshot[]
  attachments: VolumeAttachment[]
}
export interface VolumeSnapshot {
  snapshot_id: string
  generation_id: string
  source_revision: number
  capacity_bytes: number
  volume_id?: string
  volume_revision?: number
}
export interface VolumeAttachmentSpec {
  volume_id: string
  device_key: string
  mode?: "rw" | "ro"
  snapshot_id?: string
  fork_policy?: "clone" | "share_immutable" | "reject"
  capture_policy?: "include" | "external"
  expected_volume_revision?: number
}
export interface VolumeAttachment extends Pick<VolumeAttachmentSpec, "device_key"> {
  attachment_id: string
  instance_id: string
  mode: "rw" | "ro"
  fork_policy: "clone" | "share_immutable" | "reject"
  capture_policy: "include" | "external"
  state: "reserved" | "attached" | "unknown"
  revision: number
  generation_id?: string
  volume_id?: string
  machine_id?: string
  binding_id?: string
}
export interface Token extends TokenSpec {
  token_id: string
  revision: number
  created_at: string
  revoked_at?: string
  token?: string
}
export interface Mutation { epoch: string; key: string }
export interface WriteOptions { method?: string; revision?: number; intent?: Mutation; signal?: AbortSignal }
export interface MachineWriteOptions extends WriteOptions { generation?: number }
export interface PrivatePacketWakeRule {
  rule_id: string
  source_attachment_id: string
  target_attachment_id: string
  transport: "tcp" | "udp"
  guest_port: number
  enabled: boolean
  revision: number
  source_current: boolean
  source_incarnation_id?: string | null
  source_execution_epoch?: number | null
  last_wake?: {
    operation_id: string
    rule_revision: number
    execution_epoch: number
    authorized: boolean
    datagram?: "forwarded" | "dropped" | "unknown" | null
  } | null
}
export interface Page<T> { items: T[]; next_cursor?: string }
export interface TaskChunk { start: number; next: number; gap: boolean; bytes: Uint8Array }
export interface PlatformEvent { id: string; type: string; data: {
  id: string; control_epoch: string; seq: number; type: string; created_at: string; payload: unknown
  machine_id?: string; operation_id?: string; event_name?: string; resource_kind?: string; resource_id?: string; schema_version?: number
} }
export interface StreamTicket { ticket: string; protocol?: string }
export const terminalStates = new Set<OperationState>(["succeeded", "failed", "cancelled", "recovery_required"])

export class PlatformError extends Error {
  constructor(readonly status: number, readonly detail: ErrorDetail) {
    super(detail.message ?? `HTTP ${status}`)
    this.name = "PlatformError"
  }
}

const segment = encodeURIComponent
export function machine(id: string) { return "machines/" + segment(id) }
function packetWakeRule(id: string, source: string, transport: "tcp" | "udp", port: number) {
  return machine(id) + "/network/wake-rules/" + segment(source) + "/" + transport + "/" + port
}
export function task(id: string) { return "tasks/" + segment(id) }
export function snapshot(id: string) { return "snapshots/" + segment(id) }
export function volume(id: string) { return "volumes/" + segment(id) }
export function volumeSnapshot(id: string) { return "volume-snapshots/" + segment(id) }
export function service(id: string) { return "services/" + segment(id) }
export function operation(id: string) { return "operations/" + segment(id) }

function pause(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    signal?.throwIfAborted()
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", abort)
      resolve()
    }, ms)
    const abort = () => {
      clearTimeout(timer)
      reject(signal?.reason)
    }
    signal?.addEventListener("abort", abort, { once: true })
  })
}

function withQuery(path: string, params: Record<string, string>) {
  return path + (path.includes("?") ? "&" : "?") + new URLSearchParams(params)
}

export class Client {
  private readonly base: URL
  private csrf?: string

  constructor(url: string, private readonly token?: string) {
    this.base = new URL(url.replace(/\/$/, "") + "/v1/")
  }

  private url(path: string): URL {
    const pathname = path.split("?")[0]
    if (!pathname || pathname.startsWith("/") || /[:\\]/.test(pathname) ||
        pathname.split("/").some(part => [".", ".."].includes(decodeURIComponent(part)))) {
      throw new Error("Expected a relative API path")
    }
    const url = new URL(path, this.base)
    if (url.origin !== this.base.origin || !url.pathname.startsWith(this.base.pathname)) {
      throw new Error("Expected a relative API path")
    }
    return url
  }

  async response(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers)
    if (this.token) headers.set("Authorization", `Bearer ${this.token}`)
    if (this.csrf && !["GET", "HEAD"].includes(init.method ?? "GET")) {
      headers.set("X-CSRF-Token", this.csrf)
    }
    const response = await fetch(this.url(path), { ...init, headers, redirect: "error", credentials: "same-origin" })
    if (!response.ok) {
      const doc = await response.json().catch(() => ({}))
      throw new PlatformError(response.status, doc?.error ?? {})
    }
    return response
  }

  private async json<T>(path: string, init?: RequestInit): Promise<T> {
    const response = await this.response(path, init)
    if (response.status === 204) return undefined as T
    return response.json()
  }

  get<T>(path: string, signal?: AbortSignal) { return this.json<T>(path, { signal }) }

  async control(signal?: AbortSignal): Promise<Control> {
    const control = await this.get<Control>("control", signal)
    this.csrf = control.csrf_token
    return control
  }

  async login(username: string, password: string) {
    const login = await this.json<{ csrf_token: string }>("auth/login", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username, password })
    })
    this.csrf = login.csrf_token
    return login
  }

  async logout() {
    await this.response("auth/logout", { method: "POST" })
    this.csrf = undefined
  }

  async intent(signal?: AbortSignal): Promise<Mutation> {
    const control = await this.control(signal)
    return { epoch: control.control_epoch, key: crypto.randomUUID() }
  }

  private async writeHeaders(options: WriteOptions): Promise<Headers> {
    const intent = options.intent ?? await this.intent(options.signal)
    const headers = new Headers({ "If-Control-Epoch": intent.epoch, "Idempotency-Key": intent.key })
    if (options.revision !== undefined) headers.set("If-Match", `"${options.revision}"`)
    return headers
  }

  async mutate<T = Operation>(path: string, body: unknown = {}, options: WriteOptions = {}): Promise<T> {
    const headers = await this.writeHeaders(options)
    headers.set("Content-Type", "application/json")
    return this.json<T>(path, {
      method: options.method ?? "POST", headers, body: JSON.stringify(body), signal: options.signal
    })
  }

  async upload<T = Operation>(path: string, blob: Blob, options: WriteOptions & { sha256?: string } = {}): Promise<T> {
    const headers = await this.writeHeaders(options)
    headers.set("Content-Type", "application/octet-stream")
    if (options.sha256) headers.set("X-Content-SHA256", options.sha256)
    return this.json<T>(path, { method: options.method ?? "PUT", headers, body: blob, signal: options.signal })
  }

  async waitOperation(id: string, { timeoutMs = 180000, signal, onUpdate }: {
    timeoutMs?: number; signal?: AbortSignal; onUpdate?: (op: Operation) => void
  } = {}): Promise<Operation> {
    const timeout = AbortSignal.timeout(timeoutMs)
    const observe = signal ? AbortSignal.any([signal, timeout]) : timeout
    try {
      for (;;) {
        const op = await this.get<Operation>(operation(id), observe)
        onUpdate?.(op)
        if (terminalStates.has(op.state)) return op
        await pause(350, observe)
      }
    } catch (error) {
      if (timeout.aborted && !signal?.aborted) throw new Error(`Timed out observing operation ${id}`)
      throw error
    }
  }

  wait(id: string, timeoutMs?: number) { return this.waitOperation(id, { timeoutMs }) }

  async *pages<T>(path: string, limit = 100, signal?: AbortSignal): AsyncGenerator<T> {
    let cursor = ""
    do {
      const page = await this.get<Page<T>>(withQuery(path, { limit: String(limit), cursor }), signal)
      yield* page.items
      if (page.next_cursor && page.next_cursor === cursor) throw new Error("Server did not advance cursor")
      cursor = page.next_cursor ?? ""
    } while (cursor)
  }

  async *taskOutput(id: string, stream = "stdout", offset = 0, signal?: AbortSignal): AsyncGenerator<TaskChunk> {
    const path = task(id) + "/output"
    for (;;) {
      const chunk = await this.get<{
        start: number; next: number; complete: boolean; gap: boolean; data_base64: string
      }>(withQuery(path, { stream, offset: String(offset), limit: "65536" }), signal)
      const bytes = Uint8Array.from(atob(chunk.data_base64 || ""), byte => byte.charCodeAt(0))
      if (chunk.next < offset || (bytes.length && chunk.next === offset)) throw new Error("Invalid task output cursor")
      if (bytes.length || chunk.gap || chunk.next !== offset) {
        yield { start: chunk.start, next: chunk.next, gap: chunk.gap, bytes }
      }
      offset = chunk.next
      if (chunk.complete) return
      if (!bytes.length) await pause(400, signal)
    }
  }

  async *events(lastEventId = "", signal?: AbortSignal): AsyncGenerator<PlatformEvent> {
    const headers = new Headers({ Accept: "text/event-stream" })
    if (lastEventId) headers.set("Last-Event-ID", lastEventId)
    const response = await this.response("events", { headers, signal })
    if (!response.body) throw new Error("Event stream has no body")
    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader()
    let pending = ""
    let id = lastEventId
    let type = "message"
    let data: string[] = []
    try {
      for (;;) {
        const { value, done } = await reader.read()
        if (done) return
        pending += value
        let newline = pending.indexOf("\n")
        while (newline >= 0) {
          const line = pending.slice(0, newline).replace(/\r$/, "")
          pending = pending.slice(newline + 1)
          if (!line) {
            if (data.length) {
              const text = data.join("\n")
              yield { id, type, data: JSON.parse(text) }
            }
            type = "message"
            data = []
          } else {
            const colon = line.indexOf(":")
            const field = colon < 0 ? line : line.slice(0, colon)
            const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "")
            if (field === "id" && !value.includes("\0")) id = value
            if (field === "event") type = value
            if (field === "data") data.push(value)
          }
          newline = pending.indexOf("\n")
        }
      }
    } finally {
      await reader.cancel().catch(() => {})
      reader.releaseLock()
    }
  }

  async browserSocket(path: string, ticketPath: string, protocol: string, signal?: AbortSignal) {
    const ticket = await this.json<StreamTicket>(ticketPath, { method: "POST", signal })
    signal?.throwIfAborted()
    const url = this.url(path)
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:"
    return new WebSocket(url, [protocol, "ticket." + ticket.ticket])
  }

  socket<T>(path: string, protocol: string, connect: (url: URL, protocol: string, headers: Headers) => T): T {
    if (!this.token) throw new Error("Bearer authentication is required for a header-based socket")
    const url = this.url(path)
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:"
    return connect(url, protocol, new Headers({ Authorization: `Bearer ${this.token}` }))
  }

  createMachine(spec: CreateMachine, options?: WriteOptions) { return this.mutate("machines", spec, options) }
  getMachine(id: string, signal?: AbortSignal) { return this.get<Machine>(machine(id), signal) }
  getMachineResources(id: string, signal?: AbortSignal) { return this.get<MachineResourceStatus>(machine(id) + "/resources", signal) }
  async configureResources(id: string, resources: ResourcePatch, options: MachineWriteOptions = {}) {
    let { revision, generation } = options
    if (revision === undefined || generation === undefined) {
      const current = await this.getMachine(id, options.signal)
      revision ??= current.revision
      generation ??= current.binding_generation
    }
    return this.mutate(machine(id), { resources, expected_generation: generation }, { ...options, method: "PATCH", revision })
  }
  listPrivateNetworks(signal?: AbortSignal) { return this.pages<PrivateNetwork>("networks", 100, signal) }
  getMachineNetwork(id: string, signal?: AbortSignal) { return this.get<MachineNetwork>(machine(id) + "/network", signal) }
  async setNetworkDefaults(defaults: NetworkDefaults, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.get<{ revision: number }>("system/settings", options.signal)).revision
    return this.mutate<NetworkDefaults & { revision: number }>(
      "system/settings", defaults, { ...options, method: "PATCH", revision })
  }
  getLifecycleStatus(id: string, signal?: AbortSignal) {
    return this.get<LifecycleStatus>(machine(id) + "/lifecycle", signal)
  }
  async setDefaultLifecyclePolicy(policy: LifecyclePolicyPatch, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.get<{ revision: number }>("system/settings", options.signal)).revision
    return this.mutate<{ revision: number; default_lifecycle_revision: number; default_lifecycle_policy: LifecyclePolicy }>(
      "system/settings", { default_lifecycle_policy: policy }, { ...options, method: "PATCH", revision })
  }
  async updateLifecyclePolicy(id: string, policy: LifecyclePolicyPatch, options: MachineWriteOptions & { policyRevision?: number } = {}) {
    const current = await this.getMachine(id, options.signal)
    const policyRevision = options.policyRevision ?? current.lifecycle_policy?.desired_revision
    if (policyRevision === undefined) throw new Error("Machine has no lifecycle policy revision")
    return this.mutate(machine(id), { lifecycle_policy: policy, expected_policy_revision: policyRevision,
      expected_generation: options.generation ?? current.binding_generation },
      { ...options, method: "PATCH", revision: options.revision ?? current.revision })
  }
  applyDefaultLifecyclePolicy(revision: number, machines: { machine_id: string; expected_revision: number }[], options?: WriteOptions) {
    return this.mutate<PolicyApplication>("lifecycle-policy-applications", { default_policy_revision: revision, machines }, options)
  }
  async acquireRunningLease(id: string, reason: string, ttlSeconds: number, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getMachine(id, options.signal)).revision
    return this.mutate<RunningLease>(machine(id) + "/leases",
      { semantics_version: 2, reason, ttl_seconds: ttlSeconds }, { ...options, revision })
  }
  async renewRunningLease(lease: RunningLease, ttlSeconds: number, options: WriteOptions = {}) {
    if (!lease.lease_token) throw new Error("A current running lease token is required")
    const next = await this.mutate<RunningLease>("leases/" + segment(lease.lease_id),
      { lease_token: lease.lease_token, ttl_seconds: ttlSeconds }, { ...options, revision: lease.revision, method: "PATCH" })
    return { ...next, lease_token: lease.lease_token }
  }
  releaseRunningLease(lease: RunningLease, options: WriteOptions = {}) {
    if (!lease.lease_token) throw new Error("A current running lease token is required")
    return this.mutate<RunningLease>("leases/" + segment(lease.lease_id),
      { lease_token: lease.lease_token }, { ...options, revision: lease.revision, method: "DELETE" })
  }
  async withActivity<T>(id: string, reason: string, run: (signal: AbortSignal) => Promise<T>, ttlSeconds = 120): Promise<T> {
    let lease = await this.acquireRunningLease(id, reason, ttlSeconds)
    const progress = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    let renewing = Promise.resolve()
    let closed = false
    const schedule = () => {
      timer = setTimeout(() => {
        renewing = this.renewRunningLease(lease, ttlSeconds).then(next => {
          lease = next
          if (!closed) schedule()
        }).catch(error => progress.abort(error))
      }, ttlSeconds * 1000 / 3)
    }
    schedule()
    let finished = false
    try {
      const result = await run(progress.signal)
      progress.signal.throwIfAborted()
      finished = true
      return result
    } finally {
      closed = true
      clearTimeout(timer!)
      await renewing
      try { await this.releaseRunningLease(lease) } catch (error) { if (finished) throw error }
      if (finished) progress.signal.throwIfAborted()
    }
  }
  getPrivatePacketWakeRule(id: string, source: string, transport: "tcp" | "udp", port: number, signal?: AbortSignal) {
    return this.get<PrivatePacketWakeRule>(packetWakeRule(id, source, transport, port), signal)
  }
  async setPrivatePacketWakeRule(id: string, source: string, transport: "tcp" | "udp", port: number, enabled: boolean, ruleRevision: number, options: MachineWriteOptions = {}) {
    const current = options.revision === undefined || options.generation === undefined ? await this.getMachine(id, options.signal) : undefined
    return this.mutate<PrivatePacketWakeRule>(packetWakeRule(id, source, transport, port), {
      enabled, expected_revision: ruleRevision, expected_generation: options.generation ?? current?.binding_generation,
    }, { ...options, revision: options.revision ?? current?.revision, method: "PUT" })
  }
  getOperation(id: string, signal?: AbortSignal) { return this.get<Operation>(operation(id), signal) }
  listBackupPolicies(signal?: AbortSignal) { return this.pages<BackupPolicy>("backups/policies", 100, signal) }
  listBackups(signal?: AbortSignal) { return this.pages<BackupRun>("backups", 100, signal) }
  getBackup(id: string, signal?: AbortSignal) { return this.get<BackupRun>("backups/" + segment(id), signal) }
  runBackup(policy: string, options?: WriteOptions) {
    return this.mutate<BackupRun>("backups/policies/" + segment(policy) + "/runs", {}, options)
  }
  async retryForkTargets(id: string, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getOperation(id, options.signal)).revision
    return this.mutate<Operation>(operation(id) + "/retry", {}, { ...options, revision })
  }
  cancelOperation(id: string, options: WriteOptions = {}) {
    return this.mutate<Operation>(operation(id) + "/cancel", {}, options)
  }
  getService(id: string, signal?: AbortSignal) { return this.get<Service>(service(id), signal) }
  listDesktopSessions(id: string, signal?: AbortSignal) {
    return this.pages<DesktopSession>(machine(id) + "/desktop-sessions", 100, signal)
  }
  async createDesktopSession(id: string, serviceId: string, ttlSeconds?: number, options: MachineWriteOptions = {}) {
    const current = options.generation === undefined || options.revision === undefined ? await this.getMachine(id, options.signal) : undefined
    return this.mutate<DesktopSession>(machine(id) + "/desktop-sessions", {
      service_id: serviceId, expected_generation: options.generation ?? current?.binding_generation, ttl_seconds: ttlSeconds,
    }, { ...options, revision: options.revision ?? current?.revision })
  }
  closeDesktopSession(id: string, options?: WriteOptions) {
    return this.mutate<DesktopSession>("desktop-sessions/" + segment(id), {}, { ...options, method: "DELETE" })
  }
  desktopSocket(id: string, signal?: AbortSignal) {
    const path = "desktop-sessions/" + segment(id)
    return this.browserSocket(path + "/tunnel", path + "/tunnel-tickets", "sandbox.rfb.v1", signal)
  }
  getSnapshot(id: string, signal?: AbortSignal) { return this.get<Snapshot>(snapshot(id), signal) }
  listMachines(signal?: AbortSignal) { return this.pages<Machine>("machines", 100, signal) }
  listVolumes(signal?: AbortSignal) { return this.pages<Volume>("volumes", 100, signal) }
  createVolume(spec: VolumeSpec, options?: WriteOptions) { return this.mutate("volumes", spec, options) }
  getVolume(id: string, signal?: AbortSignal) { return this.get<Volume>(volume(id), signal) }
  async growVolume(id: string, capacityBytes: number, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision
    return this.mutate(volume(id), { capacity_bytes: capacityBytes }, { ...options, method: "PATCH", revision })
  }
  async deleteVolume(id: string, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision
    return this.mutate(volume(id), {}, { ...options, method: "DELETE", revision })
  }
  listVolumeSnapshots(id: string, signal?: AbortSignal) {
    return this.pages<VolumeSnapshot>(volume(id) + "/snapshots", 100, signal)
  }
  getVolumeSnapshot(id: string, signal?: AbortSignal) {
    return this.get<VolumeSnapshot & { volume_id: string; volume_revision: number }>(volumeSnapshot(id), signal)
  }
  async createVolumeSnapshot(id: string, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision
    return this.mutate(volume(id) + "/snapshots", {}, { ...options, revision })
  }
  async deleteVolumeSnapshot(id: string, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getVolumeSnapshot(id, options.signal)).volume_revision
    return this.mutate(volumeSnapshot(id), {}, { ...options, method: "DELETE", revision })
  }
  cloneVolumeSnapshot(id: string, name: string, options?: WriteOptions) {
    return this.mutate(volumeSnapshot(id) + "/clones", { name }, options)
  }
  listVolumeAttachments(id: string, signal?: AbortSignal) {
    return this.pages<VolumeAttachment & { volume_id: string; machine_id: string; binding_id: string }>(
      machine(id) + "/volume-attachments", 100, signal)
  }
  async attachVolume(id: string, spec: VolumeAttachmentSpec, options: MachineWriteOptions = {}) {
    const expectedVolumeRevision = spec.expected_volume_revision ?? (await this.getVolume(spec.volume_id, options.signal)).revision
    return this.machineMutation(id, "volume-attachments", { ...spec, expected_volume_revision: expectedVolumeRevision }, options)
  }
  detachVolume(id: string, volumeRevision: number, options: MachineWriteOptions & { generation: number; revision: number }) {
    return this.mutate("volume-attachments/" + segment(id),
      { expected_volume_revision: volumeRevision, expected_generation: options.generation }, { ...options, method: "DELETE" })
  }
  listRuntimeProfiles(signal?: AbortSignal) { return this.pages<RuntimeProfile>("runtime-profiles", 100, signal) }
  machineAction(id: string, action: "start" | "stop" | "pause" | "resume" | "wake", generation: number, options: WriteOptions) {
    return this.mutate(machine(id) + "/" + action, { expected_generation: generation }, options)
  }
  private async machineMutation(id: string, suffix: string, body: object, options: MachineWriteOptions = {}) {
    const current = options.revision === undefined || options.generation === undefined
      ? await this.getMachine(id, options.signal) : undefined
    return this.mutate(machine(id) + "/" + suffix,
      { ...body, expected_generation: options.generation ?? current?.binding_generation },
      { ...options, revision: options.revision ?? current?.revision })
  }
  standby(id: string, activityPolicy?: ActivityPolicy, options?: MachineWriteOptions) {
    return this.machineMutation(id, "standby", { activity_policy: activityPolicy }, options)
  }
  hibernate(id: string, activityPolicy?: ActivityPolicy, options?: MachineWriteOptions) {
    return this.machineMutation(id, "hibernate", { activity_policy: activityPolicy }, options)
  }
  startTask(id: string, spec: TaskSpec, options?: MachineWriteOptions) {
    return this.machineMutation(id, "tasks", spec, options)
  }
  getTask(id: string, signal?: AbortSignal) { return this.get<Task>(task(id), signal) }
  async cancelTask(id: string, graceSeconds = 5, options: WriteOptions = {}) {
    let revision = options.revision
    if (revision === undefined) {
      const current = await this.getTask(id, options.signal)
      revision = (await this.getMachine(current.machine_id, options.signal)).revision
    }
    return this.mutate(task(id) + "/cancel", { grace_seconds: graceSeconds }, { ...options, revision })
  }
  capture(id: string, purpose: "clone" | "recovery", label: string, { captureMode, ...options }: MachineWriteOptions & { captureMode?: CaptureMode } = {}) {
    return this.machineMutation(id, "snapshots", { purpose, label, capture_mode: captureMode }, options)
  }
  checkpoint(id: string, label = "", options?: MachineWriteOptions) {
    return this.machineMutation(id, "snapshots", { label, capture_mode: "vm" }, options)
  }
  clone(id: string, childName: string, options?: MachineWriteOptions) {
    return this.machineMutation(id, "clones", { child_name: childName, copy_policy: "managed_clean" }, options)
  }
  fork(id: string, childName: string, { requireIndependent, ...options }: MachineWriteOptions & { requireIndependent?: boolean } = {}) {
    return this.machineMutation(id, "forks", { child_name: childName, require_independent: requireIndependent || undefined }, options)
  }
  forkBatch(id: string, request: ForkBatch, options?: MachineWriteOptions) {
    return this.machineMutation(id, "fork-batches", { require_independent: true, ...request }, options)
  }
  restore(snapshotID: string, name: string, { volumeRestorePolicy, ...options }: WriteOptions & { volumeRestorePolicy?: VolumeRestorePolicy } = {}) {
    return this.mutate("restores", { snapshot_id: snapshotID, name, volume_restore_policy: volumeRestorePolicy }, options)
  }
  rollbackVM(id: string, snapshotID: string, volumeRestorePolicy: VolumeRestorePolicy = "clone_conflicts", options?: MachineWriteOptions) {
    return this.machineMutation(id, "rollback", { snapshot_id: snapshotID, volume_restore_policy: volumeRestorePolicy }, options)
  }
  listTokens(signal?: AbortSignal) { return this.pages<Token>("tokens", 100, signal) }
  issueToken(spec: TokenSpec, options?: WriteOptions) { return this.mutate<Token>("tokens", spec, options) }
  revokeToken(id: string, revision: number, options: WriteOptions = {}) {
    return this.mutate<{ token_id: string; revision: number; status: "revoked" }>(
      "tokens/" + segment(id), {}, { ...options, method: "DELETE", revision }
    )
  }
  async expose(id: string, spec: ServiceSpec, options: WriteOptions = {}) {
    const revision = options.revision ?? (await this.getMachine(id, options.signal)).revision
    return this.mutate(machine(id) + "/services", spec, { ...options, revision })
  }
}
