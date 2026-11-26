import { Client, PlatformError } from "../../../sdk/typescript/src/index"
import type {
  Capability, Control, CreateMachine, GuestFileEntry, Machine, MachineMetrics, Operation, PowerState,
  PrivateNetwork, RuntimeProfile, SystemMetrics,
} from "../../../sdk/typescript/src/index"

const epoch = "fixture-console"
const observed = "2026-11-25T15:00:00Z"
const atlas = "fixture-atlas"
const cedar = "fixture-cedar"
const fork = "fixture-fork"
const supported: Capability = { supported: true, status: "supported" }
const capabilities: Record<string, Capability> = {
  "boot.system": supported,
  "boot.live_compute_growth": supported,
  "state.independent_fork": supported,
  "state.capture_vm": supported,
  "state.checkpoint": supported,
  "state.clone_vm": supported,
  "state.restore_vm": supported,
  "state.standby": supported,
  "state.hibernate": supported,
  "files.list": supported,
  "files.read": { ...supported, constraints: { max_bytes: 67108864, transport: "native_agent" } },
  "files.write": { ...supported, constraints: { max_bytes: 67108864, transport: "native_agent" } },
  "files.preview": { ...supported, constraints: { max_bytes: 262144, transport: "native_agent" } },
  "files.edit": { ...supported, constraints: { max_bytes: 262144, transport: "native_agent" } },
}
const names: Record<string, string> = { [atlas]: "atlas-dev", [fork]: "atlas-dev-fork", [cedar]: "cedar-lab" }
const network: PrivateNetwork = { network_id: "fixture-private", runtime_instance_id: "fixture-worker", cidr: "10.80.0.0/24" }
const control: Control & { runtime_ready: boolean; principal: { username: string } } = {
  control_epoch: epoch, runtime_ready: true, default_boot_mode: "system",
  principal: { username: "Taylor" }, runtime_version: "Fixture", capabilities,
}
const profile: RuntimeProfile = {
  runtime_instance_id: network.runtime_instance_id, runtime_bundle_id: "fixture-runtime", state_reader_id: "fixture-reader",
  status: "registered", verification_status: "verified", security_status: "verified", boot_profiles: ["system", "oci"],
  running_bindings: 2, standby_bindings: 0, hibernated_bindings: 0, bundle_binding_refs: 3,
  bundle_snapshot_refs: 0, capabilities,
}
const fixtureTemplate = {
  template_id: "fixture-linux", name: "Debian", version: 1, guest_release: "Debian",
  spec: { boot_mode: "system" }, image_digest: "fixture",
}
const platform = { active_access_holds: 0, wake_waiters: 0, operations_by_state: {} }
const emptyPaths = new Set([
  "backups", "backups/policies", "client-certificates", "operations", "restores", "snapshots", "ssh-keys", "tokens", "volumes",
])
const emptyMachinePaths = new Set([
  "backups", "dependencies", "desktop-sessions", "execs", "leases", "services", "snapshots", "tasks", "volume-attachments",
])
const stoppedScenes = new Set(["04", "15", "19", "21", "22", "23", "24", "25", "26", "27", "28", "29"])
const desktopRegionalScenes = new Set(["02", "03", "04", "15", "19", "36"])
const mobileRegionalScenes = new Set(["41", "42", "43", "44", "45"])
const namedPowers: Record<string, PowerState> = { standby: "standby", hibernated: "hibernated", unknown: "unknown" }
const fixtureFiles: GuestFileEntry[] = [
  { name: "projects", type: "directory", size: 4096 },
  { name: "README.md", type: "file", size: 36 },
  { name: "app-config.json", type: "file", size: 215 },
  { name: "sample.bin", type: "file", size: 4 },
]

function fixtureMachine(id: string, state: PowerState, name = names[id] ?? "example-machine", guestUnavailable = false): Machine {
  const resources = { cpu_count: id === cedar ? 2 : 1, memory_mib: 4096, storage_gib: 32 }
  const guest = state === "running" && !guestUnavailable ? "ready" : "unavailable"
  return {
    machine_id: id, name, revision: 1, binding_generation: 1,
    boot_mode: "system", desktop_profile: "software-vnc-v1", template_id: fixtureTemplate.template_id,
    runtime_instance_id: network.runtime_instance_id, power_state: state, observed_state: state,
    desired_power: state, availability: state === "running" ? "available" : "unavailable",
    observed_at: observed, last_user_activity_at: observed, resources, spec: { resources },
    health: { vm: state, guest_management: guest, workload: guest, network_identity: guest, services: "unknown" },
    guest_management_status: guest, binding: {
      binding_id: id + "-binding", execution_epoch: 1, incarnation_id: id + "-execution",
      runtime_instance_id: network.runtime_instance_id, runtime_bundle_id: "fixture-runtime",
    },
    capabilities: guestUnavailable
      ? { ...capabilities, "files.list": { supported: false, status: "unsupported", reason_code: "GUEST_FILE_TRANSFER_UNSUPPORTED" } }
      : capabilities,
  }
}

const json = (data: unknown, status = 200) => Response.json(data, { status })
const page = (items: unknown[], next_cursor?: string) => json({ items, next_cursor })

function delay(signal?: AbortSignal | null) {
  return new Promise<never>((_, reject) => {
    signal?.throwIfAborted()
    signal?.addEventListener("abort", () => reject(signal.reason), { once: true })
  })
}

export class FixtureClient extends Client {
  private machines: Machine[]
  private creates = new Map<string, { operation: Operation; request: CreateMachine; reads: number }>()

  constructor(private readonly scene: string) {
    super(location.origin)
    const state = namedPowers[scene] ?? (stoppedScenes.has(scene) ? "stopped" : "running")
    const ids = scene === "empty" ? []
      : mobileRegionalScenes.has(scene) ? [atlas, fork, cedar]
        : scene === "46" ? [atlas, fork]
          : scene === "01" || desktopRegionalScenes.has(scene) ? [atlas, cedar]
            : [atlas]
    this.machines = ids.map(id => fixtureMachine(id, state, undefined, scene === "guest-unavailable" || scene === "files-unavailable"))
  }

  private create(init: RequestInit) {
    const key = new Headers(init.headers).get("Idempotency-Key")
    if (!key) throw new PlatformError(400, { code: "FIXTURE_MISSING_INTENT" })
    const existing = this.creates.get(key)
    if (existing) return json(existing.operation, 202)
    const request = JSON.parse(String(init.body)) as CreateMachine
    const id = "fixture-operation-" + (this.creates.size + 1)
    const operation: Operation = {
      operation_id: id, control_epoch: epoch, kind: "create", stage: "accepted",
      state: "queued", effect: "none", revision: 1, created_at: observed, updated_at: observed,
    }
    this.creates.set(key, { operation, request, reads: 0 })
    return json(operation, 202)
  }

  private operation(id: string) {
    const receipt = [...this.creates.values()].find(entry => entry.operation.operation_id === id)
    if (!receipt) throw new PlatformError(404, { code: "FIXTURE_OPERATION_MISSING" })
    if (receipt.operation.state !== "queued" || this.scene === "create-pending" || ++receipt.reads < 2) return receipt.operation
    const machineId = "fixture-created-" + id.replace("fixture-operation-", "")
    if (this.scene === "create-failed" || this.scene === "create-partial") {
      receipt.operation = {
        ...receipt.operation, state: "failed", stage: "complete", revision: 2,
        effect: this.scene === "create-partial" ? "partial" : "none",
        error: { code: "RESOURCE_EXHAUSTED", message: "The fixture worker is out of capacity." },
      }
    } else {
      this.machines.push(fixtureMachine(machineId, "running", receipt.request.name))
      receipt.operation = { ...receipt.operation, state: "succeeded", stage: "complete", revision: 2, effect: "applied", machine_id: machineId }
    }
    return receipt.operation
  }

  async response(path: string, init: RequestInit = {}): Promise<Response> {
    init.signal?.throwIfAborted()
    const target = new URL(path, location.origin + "/v1/")
    const key = target.pathname.slice("/v1/".length)
    const method = init.method ?? "GET"
    if (key === "events") return delay(init.signal)
    if (key === "auth/logout") return new Response(null, { status: 204 })
    if (method === "POST" && key === "machines") return this.create(init)
    if (method !== "GET") throw new PlatformError(501, { code: "FIXTURE_ACTION_UNAVAILABLE", message: "This action requires a connected platform." })
    if (key === "control") return json(control)
    if (key === "machines") {
      if (this.scene === "loading") return delay(init.signal)
      if (this.scene === "error") throw new PlatformError(503, { code: "RUNTIME_UNAVAILABLE", message: "The worker is unavailable." })
      return page(this.machines)
    }
    if (key === "templates") return page([fixtureTemplate])
    if (key === "runtime-profiles") return page([profile])
    if (key === "networks") return page([network])
    if (key === "system/settings") return json({ revision: 1, default_idle_seconds: 1800, default_boot_mode: "system", default_network_mode: "native" })
    if (key === "system/diagnostics") return json({ runtime_ready: true })
    if (key === "system/metrics") return json({
      availability: "unavailable", source: "worker", scope: "worker", platform,
    } satisfies SystemMetrics)
    if (key === "operations") return page([...this.creates.values()].map(entry => entry.operation))
    if (key.startsWith("operations/")) return json(this.operation(decodeURIComponent(key.slice("operations/".length))))
    if (emptyPaths.has(key)) return page([])
    const [collection, id, resource, subresource] = key.split("/")
    if (collection === "machines" && id) {
      const machine = this.machines.find(machine => machine.machine_id === decodeURIComponent(id))
      if (!machine) throw new PlatformError(404, { code: "MACHINE_NOT_FOUND", message: "The fixture machine is unavailable." })
      if (!resource) return json(machine)
      if (emptyMachinePaths.has(resource)) return page([])
      if (resource === "resources") return json({
        machine_id: machine.machine_id, binding_generation: machine.binding_generation,
        power_state: machine.power_state, requested: machine.resources, issues: {},
      })
      if (resource === "metrics") {
        const availability = this.scene === "metrics-stale" ? "stale" : this.scene === "metrics-partial" ? "partial"
          : this.scene === "metrics-unavailable" || machine.power_state !== "running" ? "unavailable" : "available"
        const sampled = availability !== "unavailable"
        const value = this.scene === "metrics-zero" ? 0 : 240
        return json({
          availability, source: "worker", scope: "machine", platform,
          ...(sampled ? {
            observed_at: observed, cpu_millis: value, memory_mb: availability === "partial" ? null : 746, disk_used_mb: 1880,
          } : { issues: { runtime: { code: "METRICS_UNAVAILABLE", message: "No sample for this execution." } } }),
        } satisfies MachineMetrics)
      }
      if (resource === "files" && subresource === "list") {
        if (this.scene === "files-denied") throw new PlatformError(403, { code: "PERMISSION_DENIED", message: "Access to this folder was denied." })
        if (this.scene === "files-stale") throw new PlatformError(409, { code: "GENERATION_CONFLICT", message: "The machine execution changed." })
        if (machine.power_state !== "running" || this.scene === "files-unavailable") {
          throw new PlatformError(409, { code: "GUEST_FILE_TRANSFER_UNSUPPORTED", message: "File access is unavailable for this execution." })
        }
        if (this.scene === "files-empty") return page([])
        if (this.scene === "files-paginated") return target.searchParams.get("cursor") ? page(fixtureFiles.slice(2)) : page([
          ...fixtureFiles.slice(0, 2),
          { name: "a-long-filename-for-testing-ellipsis-and-full-keyboard-access.md", type: "file", size: 512 },
        ], "fixture-page-two")
        return page(fixtureFiles)
      }
      if (resource === "files") {
        if (this.scene === "files-too-large") throw new PlatformError(413, { code: "FILE_TOO_LARGE", message: "The file exceeds the preview limit." })
        if (target.searchParams.get("path")?.endsWith("sample.bin")) return new Response(Uint8Array.of(0, 255, 10, 65))
        return new Response("# Atlas workspace\nDevelopment files\n")
      }
    }
    throw new PlatformError(404, { code: "FIXTURE_ROUTE_UNAVAILABLE", message: "No fixture is defined for this request." })
  }
}
