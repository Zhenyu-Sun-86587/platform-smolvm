import { lazy, Suspense, useEffect, useRef, useState } from "react"
import type { Machine, MachineNetwork, MachineResourceStatus, Page, ResourcePatch, RunningLease, Service, Snapshot, Task } from "../../sdk/typescript/src/index"
import { machine as machinePath, service, snapshot, task } from "../../sdk/typescript/src/index"
import type { Actions } from "./main"
import { api, argv, bundleBound, Callout, Details, download, fullStateBlock, label, machinePowerState, networkModes, num, Section, segment, State, str, time, useRemote } from "./common"
import { Cpu, HardDrive, MemoryStick, Moon, Layers3, Ellipsis, Globe2, Pencil } from "lucide-react"
import styles from "./features/machine/detail.module.css"
import { ResourceUsage } from "./features/machine/ResourceUsage"
import { WorkspaceGuide } from "./features/machine/WorkspaceGuide"
import { Files } from "./features/files/FileBrowser"
import { fileDigest } from "./features/files/digest"
export { fileDigest } from "./features/files/digest"
import { Button } from "./daytona/button"
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuItem, DropdownMenuContent } from "./daytona/dropdown-menu"
import { currentServiceObservation, fixtureScene, useCollection } from "./api"
import { machineHref, terminalHref } from "./routes"
import type { Field } from "./common"
import { policyFields, policyOverrides } from "./lifecycle-policy"
import { powerDialog, WorkspaceActions } from "./workspace-actions"
import { Branches } from "./branches"
const Terminal = lazy(() => import("./Terminal"))
const Desktop = lazy(() => import("./Desktop"))

type Context = { machine: Machine; actions: Actions; revision: number }
const tabs = ["overview", "networking", "checkpoints", "backups", "forks", "automations", "files", "agents", "settings"]
const aliases: Record<string, string> = { services: "networking", snapshots: "checkpoints", branches: "forks", tasks: "automations", activity: "settings" }
const featureKeys = ["live_fork", "independent_clone", "clean_checkpoint", "active_recovery", "persistent_task", "automatic_idle"]
const commandFields = [
  { name: "argv", label: "Command arguments (JSON array)", type: "textarea" as const, value: '["/bin/sh"]', required: true },
  { name: "cwd", label: "Guest working directory", value: "/workspace" },
  { name: "user", label: "Guest user", value: "root" }
]
const command = (form: FormData) => ({ argv: argv(form), cwd: str(form, "cwd"), user: str(form, "user") })
const taskID = (value: Task) => value.task_id ?? value.id ?? ""

export function capabilityReason(machine: Machine | undefined, feature: string) {
  const capability = machine?.capabilities?.[feature]
  if (capability?.supported !== false) return undefined
  return capability.message ?? capability.reason_code ?? "This machine does not currently support the action."
}

function FeatureStatus({ reason }: { reason?: string }) {
  return reason ? <p className="feature-status" role="status">{reason}</p> : null
}

export function MachineView({ tab, ...context }: Context & { tab: string }) {
  const { machine, actions } = context
  const base = machineHref(machine.machine_id)
  const current = aliases[tab] ?? tab
  const selected = [...tabs, "desktop", "terminal"].includes(current) ? current : "overview"
  const navigation = useRef<HTMLElement>(null)
  useEffect(() => {
    const nav = navigation.current
    const active = nav?.querySelector<HTMLAnchorElement>('a[aria-current="page"]')
    if (!nav || !active) return
    const panel = nav.getBoundingClientRect()
    const link = active.getBoundingClientRect()
    if (link.left < panel.left) nav.scrollLeft += link.left - panel.left
    else if (link.right > panel.right) nav.scrollLeft += link.right - panel.right
  }, [selected])
  const guest = machine.health?.guest_management ?? machine.guest_management_status
  const resources = machine.resources ?? machine.spec?.resources
  return <article className={styles.detail} aria-label={machine.name}>
    <header className={styles.header}>
      <div className={styles.identity}>
        <div className={styles.headline}>
          <h1>{machine.name}</h1>
          <button type="button" className={styles.rename} aria-label="Rename machine" title="Rename machine" onClick={() => renameMachine(machine, actions)}><Pencil size={13} /></button>
          <State value={machinePowerState(machine)} />
        </div>
        <div className={styles.machineMeta}>
          {resources?.cpu_count != null && <span>{resources.cpu_count} vCPU</span>}
          {resources?.memory_mib != null && <span>{resources.memory_mib / 1024} GiB</span>}
          {resources?.storage_gib != null && <span>{resources.storage_gib} GiB</span>}
          <code title={machine.machine_id}>{machine.machine_id}</code>
          <span className={styles.guest}>Guest <State value={guest ?? "unknown"} /></span>
        </div>
      </div>
      <WorkspaceActions key={machine.machine_id} machine={machine} actions={actions} surface="detail"
        capture={() => captureMachine(machine, actions, bundleBound(machine))}
        captureBlock={bundleBound(machine) ? checkpointBlocked(machine) :
          capabilityReason(machine, "clean_checkpoint") && capabilityReason(machine, "active_recovery")} />
    </header>
    <nav ref={navigation} className={styles.tabs} aria-label="Machine sections">
      {tabs.map(item => <a aria-current={selected === item ? "page" : undefined} href={base + "/" + item} key={item}>{label(item)}</a>)}
    </nav>
    {machine.blocked_reasons?.length ? <div className="blockers">{machine.blocked_reasons.map(reason => <span key={reason}>{label(reason)}</span>)}</div> : null}
    <div className={styles.content} key={machine.machine_id}>
      {selected === "desktop" && machine.desktop_profile === "software-vnc-v1" ? <Suspense fallback={<p role="status">Loading desktop…</p>}><Desktop machine={machine} revision={context.revision} embedded /></Suspense>
        : selected === "terminal" ? <Execs {...context} />
        : selected === "automations" ? <><Tasks {...context} initialTask={location.hash.split("/")[4]} /><details className={styles.more}><summary>Automatic machine lifecycle</summary><Activity {...context} policyOnly /></details></>
        : selected === "files" ? <Files {...context} />
        : selected === "networking" ? <Services {...context} />
        : selected === "forks" ? <Branches {...context} />
        : selected === "checkpoints" ? <Snapshots {...context} />
        : selected === "backups" ? <Backups />
        : selected === "agents" ? <WorkspaceGuide label="Coding agents" title="Agent sessions"
          description="Agent session history is unavailable. Run a command in the guest and follow its output with persistent tasks."
          actions={<a className={styles.documentButton} href={base + "/automations"}>View persistent tasks</a>}
          commands={["# List persistent tasks", "sandboxctl tasks list " + machine.machine_id,
            "", "# Run a short Guest command", "sandboxctl exec " + machine.machine_id + " -- /usr/bin/id"]} />
        : selected === "settings" ? <div className={styles.settings}><PowerSettings machine={machine} actions={actions} /><MachineConfiguration {...context} /><Activity {...context} /></div>
        : <Overview {...context} />}
    </div>
  </article>
}

function Backups() {
  return <>
    <div className={styles.backupBanner}><div><strong>Platform backups</strong><p>Backup policies and runs are managed by the platform administrator.</p></div>
      <a className={styles.primaryLink} href="#/backups">View policies</a>
    </div>
    <WorkspaceGuide label="Backup policies" title="Disaster recovery for your machines"
      description="The administrator can run configured backup policies and review verified recovery artifacts."
      actions={<a className={styles.documentButton} href="#/backups">View backup runs</a>}
      commands={["# Show local backup commands", "sandboxctl admin backup --help"]} />
  </>
}

function renameMachine(machine: Machine, actions: Actions) {
  actions.dialog({
    title: "Rename machine", submit: "Rename",
    description: "Changes the display name for this machine.",
    fields: [{ name: "name", label: "Machine name", value: machine.name, required: true }],
    act: form => actions.mutate(machinePath(machine.machine_id), {
      name: str(form, "name"), expected_generation: machine.binding_generation,
    }, { method: "PATCH", revision: machine.revision }),
  })
}
function ManagedResources({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id)
  const result = useRemote<MachineResourceStatus>(path + "/resources", revision)
  const data = result.data
  const compute = data?.compute
  const seen = compute?.observed
  const caps = compute?.capabilities
  const fields: Field[] = []
  if (seen?.online_cpus != null && seen.cpus && (caps?.cpu_offline?.available_now || caps?.cpu_online?.available_now || caps?.cpu_grow?.available_now)) {
    fields.push({ name: "cpu_count", label: "Online vCPUs", type: "number", value: seen.online_cpus,
      min: caps.cpu_offline?.available_now ? 1 : seen.online_cpus,
      max: caps.cpu_grow?.available_now ? seen.cpus.capacity : caps.cpu_online?.available_now ? seen.cpus.created : seen.online_cpus })
  }
  const ram = seen?.ram
  const memory = seen?.memory_mib ?? (ram ? ram.boot_mib + ram.plugged / 1048576 : undefined)
  if (ram && caps?.memory_grow?.available_now) {
    fields.push({ name: "memory_mib", label: "Allocated memory (MiB)", type: "number", value: memory, min: memory,
      max: ram.boot_mib + ram.capacity / 1048576, note: "Increase in 128 MiB increments." })
  }
  if (seen?.balloon && caps?.balloon?.available_now) {
    fields.push({ name: "balloon_target_mib", label: "Balloon target (MiB)", type: "number", value: seen.balloon.target_mib, min: 0 })
  }
  if (data?.cpu_limit) {
    fields.push({ name: "cpu_limit_millicpus", label: "CPU bandwidth (millicpus)", type: "number",
      value: data.cpu_limit.requested_millicpus ?? "", min: 1, note: "Clear the field to restore the allocated bandwidth." })
  }
  const edit = () => actions.dialog({ title: "Adjust live resources", submit: "Apply", fields, act: async form => {
    const patch: ResourcePatch = {}
    for (const field of fields) {
      const raw = str(form, field.name)
      if (raw === String(field.value ?? "")) continue
      if (field.name === "cpu_limit_millicpus" && raw === "") patch.cpu_limit_millicpus = null
      else {
        const value = Number(raw)
        if (!raw || !Number.isSafeInteger(value) || value < (field.min ?? 0)) throw new Error("Enter a valid integer for " + field.label)
        Object.assign(patch, { [field.name]: value })
      }
    }
    if (!Object.keys(patch).length) throw new Error("Choose a setting to change.")
    return actions.mutate(path, { resources: patch, expected_generation: machine.binding_generation }, { method: "PATCH", revision: machine.revision })
  } })
  const quota = data?.cpu_limit
  return <Section title="Live resources" actions={fields.length > 0 && <button onClick={edit}>Adjust</button>}>
    <Callout error={result.error}/>
    {!result.error && !seen && <p className={styles.note}>
      {data ? compute?.pending?.operation_id ? "The resource change is pending." : "Live allocation has not been observed."
        : "Reading live allocation…"}
    </p>}
    {data && (seen || quota || compute?.pending) && <Details value={{
      profile: compute?.profile, online_vCPUs: seen?.online_cpus, allocated_vCPU_slots: seen?.cpus?.created,
      allocated_memory_MiB: memory, balloon_target_MiB: seen?.balloon?.target_mib, balloon_actual_MiB: seen?.balloon?.actual_mib,
      effective_millicpus: quota ? quota.quota_usec === null ? "Unlimited" : quota.quota_usec * 1000 / quota.period_usec : undefined,
      full_state_capture: compute ? compute.capture_compatible ? "Compatible" : "Unavailable for this profile" : undefined,
      pending_operation: compute?.pending?.operation_id
    }}/>}
    {data && [...new Set(Object.values(data.issues).map(issue => issue.message))].map(message => <p key={message}>{message}</p>)}
  </Section>
}

function Overview({ machine, actions, revision }: Context) {
  const resources = machine.resources ?? machine.spec?.resources
  const policy = machine.lifecycle_policy?.applied
  const hibernate = policy ? policy.auto_hibernate ? policy.hibernate_after_seconds / 60 : "Off"
    : machine.idle_policy?.enabled ? machine.idle_policy.timeout_seconds / 60 : undefined
  const keepRunning = policy?.keep_running ?? machine.keep_running
  return <>
    {fixtureScene && ["21", "37", "44"].includes(fixtureScene) && <section className={styles.fixtureUsage} aria-label="Fixture usage history">
      <div className={styles.metricsHead}><h2 className={styles.sectionTitle}>Usage · last 28 days</h2><span>Development fixture</span></div>
      <div className={styles.historyBar} data-pending={fixtureScene === "44"}><span /></div>
      <div className={styles.historyDates}><span>11/26 15:00</span><span>11/26 15:45</span></div>
    </section>}
    <ResourceUsage machine={machine} revision={revision} />
    <section aria-labelledby="machine-configured-resources">
      <h2 className={styles.sectionTitle} id="machine-configured-resources">Size</h2>
      <div className={styles.specs}>{[
        { Icon: Cpu, label: "vCPUs", value: resources?.cpu_count, unit: "" },
        { Icon: MemoryStick, label: "Memory", value: resources?.memory_mib, unit: "MiB" },
        { Icon: HardDrive, label: "Guest storage", value: resources?.storage_gib, unit: "GiB" },
        { Icon: Moon, label: "Auto-hibernate", value: keepRunning ? "Keep running" : hibernate, unit: !keepRunning && typeof hibernate === "number" ? "min" : "" },
      ].map(({ Icon, label, value, unit }) => <div className={styles.spec} key={label}>
        <span><Icon size={16} />{label}</span><strong>{value ?? "Unknown"} {unit && value != null && <small>{unit}</small>}</strong>
      </div>)}</div>
    </section>
    {bundleBound(machine) && <ManagedResources machine={machine} actions={actions} revision={revision} />}
    <Section title="Health">
      <Details value={{
        VM: machinePowerState(machine),
        guest_management: machine.health?.guest_management ?? machine.guest_management_status ?? "Unknown",
        workload: machine.health?.workload ?? "Unknown", workload_error: machine.health?.workload_error?.message,
        network_identity: machine.health?.network_identity ?? "Unknown", services: machine.health?.services ?? "Unknown",
        last_user_activity: time(machine.last_user_activity_at),
      }} />
      {machine.operation_id && <div className="panel-footer"><a href={"#/operations/" + segment(machine.operation_id)}>View current operation</a></div>}
    </Section>
  </>
}

function PowerSettings({ machine, actions }: Pick<Context, "machine" | "actions">) {
  const power = machinePowerState(machine)
  const native = bundleBound(machine)
  const actionsForPower = power === "running" ? (native ? [
    { kind: "standby", title: "Standby", description: "Suspend in memory and retain the current host allocation.", reason: fullStateBlock(machine, "lifecycle.standby") },
    { kind: "hibernate", title: "Hibernate", description: "Save memory and guest state before releasing compute.", reason: fullStateBlock(machine, "state.hibernate") },
    { kind: "stop", title: "Cold stop", description: "End the current execution and retain the guest disk.", reason: fullStateBlock(machine, "lifecycle.stop") },
  ] : [
    { kind: "pause", title: "Hibernate", description: "Save the current execution for a later resume.", reason: capabilityReason(machine, "pause") },
    { kind: "stop", title: "Cold stop", description: "End the current execution and retain the guest disk.", reason: undefined },
  ]) : power === "stopped" ? [
    { kind: "start", title: "Start", description: "Boot the machine from its guest disk.", reason: undefined },
  ] : power === "hibernated" || power === "standby" ? [
    { kind: native || power === "standby" ? "wake" : "resume", title: "Wake", description: "Resume the saved execution.",
      reason: native && power === "standby" ? fullStateBlock(machine, "lifecycle.standby_wake") : undefined },
  ] : []
  return <section><h2 className={styles.sectionTitle}>Power</h2>
    {actionsForPower.length ? <div className={styles.powerOptions}>{actionsForPower.map(item => <div className={styles.powerOption} key={item.kind}>
      <div><strong>{item.title}</strong><p>{item.reason ?? item.description}</p></div>
      <Button size="sm" variant="outline" disabled={Boolean(item.reason)} onClick={() => powerDialog(machine, actions, item.kind)}>{item.title}</Button>
    </div>)}</div> : <p className={styles.note}>Power actions are unavailable while the machine state is unknown.</p>}
  </section>
}

function MachineConfiguration({ machine, actions }: Context) {
  const resources = machine.resources ?? machine.spec?.resources
  return <>
    <section><h2 className={styles.sectionTitle}>Name</h2><div className={styles.powerOption}>
      <div><strong>Machine name</strong><p>Change the display name used in the workspace. The machine ID stays the same.</p></div>
      <Button size="sm" variant="outline" onClick={() => renameMachine(machine, actions)}>Rename</Button>
    </div></section>
    <details className={styles.more}><summary>Machine configuration and capabilities</summary>
    <Section title="Machine configuration" actions={<button onClick={() => actions.dialog({
      title: "Edit machine", submit: "Save",
      fields: bundleBound(machine) ? [{ name: "name", label: "Name", value: machine.name, required: true }] : [
        { name: "name", label: "Name", value: machine.name, required: true },
        { name: "cpu_count", label: "vCPUs", value: resources?.cpu_count, type: "number", min: 1 },
        { name: "memory_mib", label: "Memory (MiB)", value: resources?.memory_mib, type: "number", min: 1 },
        { name: "storage_gib", label: "Storage (GiB)", value: resources?.storage_gib, type: "number", min: 1 },
        { name: "overlay_gib", label: "Overlay (GiB)", value: resources?.overlay_gib, type: "number", min: 1 }
      ],
      act: form => {
        const fields = ["cpu_count", "memory_mib", "storage_gib", "overlay_gib"] as const
        const changed = fields.flatMap(key => {
          const value = num(form, key)
          return value && value !== resources?.[key] ? [[key, value]] : []
        })
        return actions.mutate(machinePath(machine.machine_id), {
          name: str(form, "name"), expected_generation: machine.binding_generation,
          ...(changed.length ? { resources: Object.fromEntries(changed) } : {})
        }, { method: "PATCH", revision: machine.revision })
      }
    })}>Edit</button>}>
      <Details value={{template:machine.template_id,boot_mode:machine.boot_mode,power_state:machinePowerState(machine),guest_management:machine.health?.guest_management ?? machine.guest_management_status,workload:machine.health?.workload,workload_error:machine.health?.workload_error?.message,network_identity:machine.health?.network_identity,services:machine.health?.services,desired_power:machine.desired_power,last_activity:time(machine.last_user_activity_at)}}/>
      <details className="disclosure"><summary>Runtime binding and identity</summary>      <Details value={{
        template: machine.template_id, desired_power: machine.desired_power,
        binding_generation: machine.binding_generation, revision: machine.revision,
        binding: machine.binding?.binding_id, runtime_name: machine.runtime_name,
        runtime_instance: machine.binding?.runtime_instance_id, runtime_bundle: machine.binding?.runtime_bundle_id,
        incarnation: machine.binding?.incarnation_id, execution_epoch: machine.binding?.execution_epoch,
        runtime_identity: machine.binding?.runtime_identity ?? machine.runtime_identity,
        runtime_state_reason: machine.binding?.state_reason ?? machine.state_reason,
        ssh_host_key: machine.binding?.guest_management_evidence?.ssh_host_key,
        last_observed: time(machine.observed_at), last_activity: time(machine.last_user_activity_at),
        guest_management: machine.guest_management_status, vCPUs: resources?.cpu_count,
        memory_MiB: resources?.memory_mib, storage_GiB: resources?.storage_gib,
        overlay_GiB: resources?.overlay_gib
      }} />
      </details>
      {machine.operation_id && <div className="panel-footer"><a href={"#/operations/" + segment(machine.operation_id)}>Operation {machine.operation_id}</a></div>}
    </Section>
    <Section title="Capabilities">
      <div className="rows">{[...new Set([...featureKeys, ...Object.keys(machine.capabilities ?? {})])].map(key => {
        const capability = machine.capabilities?.[key]
        return <div className="row" key={key}>
          <strong title={capability?.verified_profile}>{label(key)}</strong>
          <State value={capability?.status ?? (capability ? capability.supported ? "enabled" : "unavailable" : "verification_pending")} />
          <span className="muted">{capability?.message ?? capability?.reason_code ?? ""}</span>
        </div>
      })}</div>
    </Section>
    </details>
  </>
}

type Exec = { exec_id: string; state?: string; revision?: number; exit_code?: number | null; tty?: boolean }
type ExecResult = Exec & { output?: { stream: string; data: string }[]; next?: number; gap?: unknown; error?: string }
function Execs({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id) + "/execs"
  const execFields = commandFields.map(field => field.name === "cwd" && machine.boot_mode === "system" && bundleBound(machine)
    ? { ...field, value: "/" } : field)
  const sessions = useRemote<Page<Exec>>(path, revision)
  const deepLink = location.hash.split("/")[4]
  const [current, setCurrent] = useState<string | undefined>(deepLink)
  useEffect(()=>setCurrent(deepLink),[deepLink])
  const [result, setResult] = useState<ExecResult>()
  const open = () => actions.dialog({
    title: "Open terminal", fields: execFields,
    act: async form => {
      const response = await actions.mutate<Exec>(path, {
        ...command(form), tty: true, interactive: true, cols: 100, rows: 28
      }, { revision: machine.revision })
      if (response?.exec_id) location.hash = terminalHref(machine.machine_id, response.exec_id)
    }
  })
  const run = () => actions.dialog({
    title: "Run command", fields: [...execFields, { name: "wait_seconds", label: "Wait (seconds)", type: "number", value: 5, min: 0 }],
    act: async form => setResult(await actions.mutate<ExecResult>(machinePath(machine.machine_id) + "/exec", {
      ...command(form), wait_seconds: num(form, "wait_seconds")
    }, { revision: machine.revision }))
  })
  return <>
    <Section title="Native sessions" actions={<><button onClick={run}>Run command</button><button className="primary" onClick={open}>Open terminal</button></>}>
      <Callout error={sessions.error} />
      {(sessions.data?.items ?? []).length === 0 ? <p className="empty">{sessions.error ? "" : sessions.data ? "No sessions yet." : "Loading sessions…"}</p> : <div className="rows">
        {sessions.data?.items.map(exec => <div className="row" key={exec.exec_id}>
          <code>{exec.exec_id}</code><State value={exec.state} /><span>{exec.exit_code == null ? "" : `Exit ${exec.exit_code}`}</span>
          <a href={terminalHref(machine.machine_id, exec.exec_id)}>Attach</a>
          <button className="quiet-danger" onClick={() => actions.dialog({
            title: "Terminate session", destructive: true,
            act: () => actions.mutate("execs/" + segment(exec.exec_id), {}, { method: "DELETE", revision: exec.revision })
          })}>Terminate</button>
        </div>)}
      </div>}
    </Section>
    {current && <Section title="Terminal" actions={<button onClick={() => {setCurrent(undefined);location.hash="#/"+machinePath(machine.machine_id)+"/terminal"}}>Disconnect</button>}><Suspense fallback={<p className="empty">Loading terminal…</p>}><Terminal key={current} execId={current} machine={machine} embedded /></Suspense></Section>}
    {result && <Section title="Command response">
      <Details value={{ exec: result.exec_id, state: result.state, exit_code: result.exit_code, output_cursor: result.next, output_gap: result.gap, error: result.error }} />
      <pre className="record">{result.output?.map(entry => {
        const bytes = Uint8Array.from(atob(entry.data), char => char.charCodeAt(0))
        return new TextDecoder().decode(bytes)
      }).join("")}</pre>
    </Section>}
  </>
}

export function Tasks({ machine, actions, revision, initialTask }: Context & {initialTask?: string}) {
  const path = machinePath(machine.machine_id) + "/tasks"
  const blocked = capabilityReason(machine, "persistent_task")
  const history = useCollection<Task>(path, revision)
  const [selected, setSelected] = useState<string>(initialTask ?? "")
  const [stream, setStream] = useState("stdout")
  const detail = useRemote<Task>(selected ? task(selected) : "", revision)
  const start = <button className="primary" disabled={Boolean(blocked)} title={blocked} onClick={() => actions.dialog({
      title: "Start task", fields: [...commandFields, { name: "task_key", label: "Task key (optional)" }],
      act: form => actions.mutate(path, { ...command(form), expected_generation: machine.binding_generation, ...(str(form, "task_key") ? { task_key: str(form, "task_key") } : {}) }, { revision: machine.revision })
    })}>Start task</button>
  const empty = history.items && !history.items.length && (!machine.task_metadata_status || machine.task_metadata_status === "complete")
  return <>
    {empty && <WorkspaceGuide label="Persistent tasks" title="Automate work on this machine"
      description="Run a command inside this machine and follow its state and output here." actions={start} />}
    {!empty && <Section title="Persistent tasks" actions={start}>
      <FeatureStatus reason={blocked} />
      {machine.task_metadata_status === "unavailable" && <p className="empty">The Guest task index is unavailable.</p>}
      {machine.task_metadata_status === "stale" && <p className="empty">Showing the last recorded task index.</p>}
      <Callout error={history.error} />
      {history.items?.length ? <div className="rows">{history.items.map(item => <div className="row" key={taskID(item)}>
        <button className="text-button" onClick={() => setSelected(taskID(item))}>{taskID(item)}</button>
        <State value={item.state} /><span>{item.exit_code == null ? "" : `Exit ${item.exit_code}`}</span><span className="muted">{time(item.observed_at)}</span>
        {["accepted", "starting", "running", "cancelling"].includes(item.state) && <button onClick={() => actions.dialog({
          title: "Cancel task", destructive: true, fields: [{ name: "grace_seconds", label: "Grace period (seconds)", type: "number", min: 0, value: 5 }],
          act: form => actions.mutate(task(taskID(item)) + "/cancel", { grace_seconds: num(form, "grace_seconds") }, { revision: machine.revision })
        })}>Cancel</button>}
      </div>)}</div> : <p className="empty">{history.error ? "" : !history.items ? "Loading tasks…" : ""}</p>}
    </Section>}
    {selected && <Section title="Task output" actions={<select aria-label="Output stream" value={stream} onChange={event => setStream(event.target.value)}>
      <option value="stdout">stdout</option><option value="stderr">stderr</option>
    </select>}><Callout error={detail.error} />
      {detail.data && <Details value={{ task: taskID(detail.data), state: detail.data.state, exit_code: detail.data.exit_code, origin_task: detail.data.origin_task_id, origin_machine: detail.data.origin_machine_id, observed: time(detail.data.observed_at) }} />}
      <TaskOutput key={selected + stream} id={selected} stream={stream} />
    </Section>}
  </>
}

function TaskOutput({ id, stream }: { id: string; stream: string }) {
  const offset = useRef(0)
  const decoder = useRef(new TextDecoder())
  const [output, setOutput] = useState("")
  const [error, setError] = useState<unknown>()
  const [reconnect, setReconnect] = useState(0)
  const [ended, setEnded] = useState(false)
  useEffect(() => {
    const abort = new AbortController()
    setError(undefined)
    setEnded(false)
    const read = async () => {
      try {
        for await (const chunk of api.taskOutput(id, stream, offset.current, abort.signal)) {
          offset.current = chunk.next
          const reset = chunk.gap ? decoder.current.decode() + `\n[Output gap before byte ${chunk.start}]\n` : ""
          const text = decoder.current.decode(chunk.bytes, { stream: true })
          setOutput(previous => (previous + reset + text).slice(-150000))
        }
        const trailing = decoder.current.decode()
        setOutput(previous => previous + trailing)
        setEnded(true)
      } catch (error) {
        if (!abort.signal.aborted) setError(error)
      }
    }
    void read()
    return () => abort.abort()
  }, [id, stream, reconnect])
  return <div className="task-log"><div className="log-meta"><span>{ended ? "Complete" : error ? "Disconnected" : "Following"} · Byte {offset.current}</span>{error ? <button onClick={() => setReconnect(value => value + 1)}>Reconnect</button> : null}</div>
    <Callout error={error} /><pre>{output}</pre></div>
}

function readiness(service?: Service) {
  return typeof service?.readiness === "object" && service.readiness
    ? service.readiness as { type?: string; path?: string; expected_status?: number }
    : undefined
}

export function ServiceStatus({ item, machine }: { item: Service; machine?: Machine }) {
  const observation = currentServiceObservation(item, machine)
  if (item.enabled === false) return <State value="disabled" />
  if (!observation) return <State value="unknown" />
  return <div className={styles.observation}>
    <State value={observation.status} />
    <span>{label(observation.scope)} · {label(observation.stage)}</span>
    {observation.observed_at && <time dateTime={observation.observed_at}>{time(observation.observed_at)}</time>}
    {observation.reason_code && <span title={observation.reason_code}>{label(observation.reason_code.toLowerCase())}</span>}
    {observation.operation_id && <a href={"#/operations/" + segment(observation.operation_id)}>View operation</a>}
  </div>
}

export function Services({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id) + "/services"
  const list = useCollection<Service>(path, revision)
  const network = useRemote<MachineNetwork>(machine.network ? machinePath(machine.machine_id) + "/network" : "", revision)
  const [error, setError] = useState<unknown>()
  const preview = async (item: Service) => {
    try {
      const response = await api.response(service(item.service_id) + "/preview-ticket", { method: "POST" })
      const { action, ticket } = await response.json() as { action: string; ticket: string }
      const target = new URL(action)
      if (target.protocol !== "https:" || target.hostname !== item.public_host ||
          target.pathname !== "/.sandbox/session" || target.username || target.password || target.search || target.hash || !ticket) {
        throw new Error("The service preview returned an invalid destination.")
      }
      const form = document.createElement("form")
      form.method = "POST"
      form.action = action
      form.target = "_blank"
      form.rel = "noopener"
      form.hidden = true
      const input = document.createElement("input")
      input.name = "ticket"
      input.value = ticket
      form.append(input)
      document.body.append(form)
      try { form.submit() } finally { form.remove() }
    } catch (error) { setError(error) }
  }
  const configure = (current?: Service) => actions.dialog({
    title: current ? "Edit service" : "Create service", submit: current ? "Save" : "Create",
    fields: [
      { name: "name", label: "Name", value: current?.name, required: true },
      { name: "protocol", label: "Protocol", value: current?.protocol ?? "http", options: ["http", "https", "ssh", "tcp-mtls", ...(machine.boot_mode === "system" ? ["vnc"] : [])].map(value => ({ value })) },
      { name: "guest_port", label: "Guest port", type: "number", value: current?.guest_port ?? 8080, min: 1, required: true },
      { name: "readiness", label: "Readiness probe", value: readiness(current)?.type ?? "tcp", options: [{ value: "tcp", label: "TCP connection" }, { value: "http", label: "HTTP response" }] },
      { name: "health_path", label: "HTTP health path", value: readiness(current)?.path ?? "/" },
      { name: "health_status", label: "Expected HTTP status", type: "number", min: 100, value: readiness(current)?.expected_status ?? 200 }
    ],
    act: form => actions.mutate(current ? service(current.service_id) : path, {
      name: str(form, "name"), protocol: str(form, "protocol"), guest_port: num(form, "guest_port"),
      ...(current ? {} : { expected_generation: machine.binding_generation }),
      readiness: str(form, "readiness") === "http"
        ? { type: "http", path: str(form, "health_path"), expected_status: num(form, "health_status") }
        : { type: "tcp" }
    }, { method: current ? "PATCH" : "POST", revision: current?.revision ?? machine.revision })
  })
  return <>
    {machine.network && <section className={styles.networkGroup}><h2 className={styles.sectionTitle}>Networks</h2>
      <Callout error={network.error} />
      <Details value={{ network: machine.network.network_id,
        mode: networkModes.find(mode => mode.value === machine.network?.mode)?.label,
        state: network.data?.state, private_address: network.data?.private_ipv4,
        guest_address: network.data?.guest_ipv4, dns_name: network.data?.dns_name }} />
    </section>}
    <section className={styles.networkGroup}><h2 className={styles.sectionTitle}>Ports</h2>
    <Callout error={list.error} /><Callout error={error} />
    {!list.items?.length && !list.error && <p className={styles.note} role="status">{list.items ? "No services published." : "Loading services…"}</p>}
    <div className="rows">{list.items?.map(item => {
      const ssh = item.protocol === "ssh"
      const http = item.protocol === "http" || item.protocol === "https"
      const url = item.public_host && http ? `https://${item.public_host}${item.public_port ? ":" + item.public_port : ""}` : undefined
      const connect = ssh ? `sandboxctl ssh-config ${machine.machine_id}` : url
        ? `curl -H "Authorization: Bearer \$SANDBOX_TOKEN" "${url}"`
        : `sandboxctl port-forward ${item.service_id} --local-port 8080`
      return <div className={styles.port} key={item.service_id}>
        <span className={styles.portIcon}><Globe2 size={16} /></span>
        <div className={styles.portInfo}><div><strong>{item.public_host ?? item.name}</strong><code>{item.protocol}</code></div>
          <p>{item.name} · Forwards to guest port {item.guest_port}</p>
          <ServiceStatus item={item} machine={machine} />
        </div>
        <div className={styles.portActions}>
          {url && <Button size="sm" variant="outline" disabled={item.enabled === false} onClick={() => void preview(item)}>Open preview ↗</Button>}
          <Button size="sm" variant="outline" onClick={() => configure(item)}>Edit</Button>
          <DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" size="icon-sm" aria-label={"Actions for " + item.name}><Ellipsis /></Button></DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onSelect={() => navigator.clipboard.writeText(connect).catch(setError)}>Copy connection command</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => actions.dialog({ title: "Delete service", destructive: true,
                act: () => actions.mutate(service(item.service_id), {}, { method: "DELETE", revision: item.revision }),
              })}>Delete</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <details className={styles.portDetails}><summary>Configuration</summary>
          <Details value={{ host: item.public_host, public_port: item.public_port, readiness_probe: item.readiness,
            auth: item.auth, ssh_fingerprint: item.ssh_fingerprint, ssh_host_key: item.ssh_host_key, connection: connect }} />
        </details>
      </div>
    })}</div>
    <button type="button" className={styles.addPort} onClick={() => configure()}>+ Add port</button>
    </section>
  </>
}

function checkpointBlocked(machine: Machine) {
  const power = machinePowerState(machine)
  return fullStateBlock(machine, "state.checkpoint") ??
    (power === "running" || power === "standby" ? undefined : "Start this machine before capturing its execution.")
}

function captureMachine(machine: Machine, actions: Actions, vm: boolean) {
  const clean = capabilityReason(machine, "clean_checkpoint")
  const recovery = capabilityReason(machine, "active_recovery")
  actions.dialog({
    title: vm ? "Capture VM checkpoint" : "Capture snapshot", submit: "Capture",
    fields: [{ name: "label", label: "Label" }, ...(!vm ? [{
      name: "purpose", label: "Purpose", value: clean ? "recovery" : "clone", options: [
        { value: "clone", label: "Clean clone", disabled: Boolean(clean) },
        { value: "recovery", label: "Recovery", disabled: Boolean(recovery) },
      ],
    }] : [])],
    act: form => actions.mutate(machinePath(machine.machine_id) + "/snapshots", {
      label: str(form, "label"), expected_generation: machine.binding_generation,
      ...(vm ? { capture_mode: "vm" } : { purpose: str(form, "purpose") }),
    }, { revision: machine.revision }),
  })
}

export function Snapshots({ machine, machines, actions, revision, directory = false, items }: {
  machine?: Machine; machines?: Machine[]; actions: Actions; revision: number; directory?: boolean; items?: Snapshot[]
}) {
  const base = machine ? machinePath(machine.machine_id) : ""
  const history = useCollection<Snapshot>(items ? "" : base ? base + "/snapshots" : "snapshots", revision)
  const captures = items ?? history.items
  const cleanBlock = capabilityReason(machine, "clean_checkpoint")
  const recoveryBlock = capabilityReason(machine, "active_recovery")
  const vmBlock = machine && checkpointBlocked(machine)
  const [error, setError] = useState<unknown>()
  const modal = (mode: string, item: Snapshot) => {
    const resource = snapshot(item.snapshot_id)
    const vm = (item.capture_mode ?? item.capture_summary?.capture_mode) === "vm"
    if (mode === "Export") {
      api.response(resource + "/export").then(response => download(response, item.snapshot_id + ".tar")).catch(setError)
      return
    }
    if (vm && (mode === "Restore" || (mode === "Rollback" && machine))) {
      const policies = item.capture_summary?.restore_policies ?? ["exact", "clone_conflicts"]
      const preferred = policies.includes("clone_conflicts") ? "clone_conflicts" : policies[0]
      actions.dialog({
        title: mode + " VM checkpoint", destructive: mode === "Rollback",
        description: mode === "Rollback" ? "The Worker saves a rescue StatePoint before replacing this execution." : undefined,
        fields: [
          ...(mode === "Restore" ? [{ name: "name", label: "New machine name", required: true }] : []),
          ...(policies.length ? [{ name: "volume_policy", label: "Volume restore policy", value: preferred, options: policies.map(value => ({
            value, label: value === "clone_conflicts" ? "Create independent writable volumes" : "Require exact volume generations",
          })) }] : []),
        ],
        act: form => {
          const body = { snapshot_id: item.snapshot_id, ...(preferred ? { volume_restore_policy: str(form, "volume_policy") } : {}) }
          if (mode === "Rollback" && machine) return actions.mutate(base + "/rollback",
            { ...body, expected_generation: machine.binding_generation }, { revision: machine.revision })
          return actions.mutate("restores", { ...body, name: str(form, "name") })
        },
      })
      return
    }
    actions.dialog({
      title: mode + " snapshot", destructive: ["Delete", "Rollback"].includes(mode),
      description: mode === "Rollback" ? "A paused source is exported and then deleted. Its prior execution is retained as an artifact." : undefined,
      fields: mode === "Rollback" ? [{ name: "task_disposition", label: "Captured tasks", options: [
        { value: "cancel_captured", label: "Cancel captured tasks" }, { value: "resume_captured", label: "Resume eligible captured tasks" }
      ] }] : mode === "Restore" ? [{ name: "name", label: "New machine name", required: true }]
        : mode === "Verify" ? [{ name: "verification", label: "Verification", options: [{ value: "checksum" }, { value: "restore" }] }] : [],
      act: form => {
        if (mode === "Rollback" && machine) return actions.mutate(base + "/rollback", {
          snapshot_id: item.snapshot_id, expected_generation: machine.binding_generation,
          old_state_policy: "export_then_delete_on_paused", task_disposition: str(form, "task_disposition")
        }, { revision: machine.revision })
        if (mode === "Restore") return actions.mutate("restores", { snapshot_id: item.snapshot_id, name: str(form, "name") })
        if (mode === "Verify") return actions.mutate(resource + "/verify", { verification: str(form, "verification") }, { revision: item.revision })
        return actions.mutate(resource, {}, { method: "DELETE", revision: item.revision })
      }
    })
  }
  const rows =
      <div className="rows">{captures?.map(item => <div key={item.snapshot_id}>
        <div className="row snapshot-row"><span className="machine-icon"><Layers3 size={17}/></span><div><strong>{item.label || item.snapshot_id}</strong><small>{time(item.created_at)}</small></div><State value={item.state}/>
          <span className="subtle">{(item.capture_mode ?? item.capture_summary?.capture_mode) === "vm" ? "VM checkpoint" : (item.purpose === "clone" ? "Clean checkpoint" : "Recovery checkpoint") + " · " + item.task_content}</span>
          {!machine && item.source_machine_id && <a href={"#/machines/"+segment(item.source_machine_id)+"/snapshots"}>{machines?.find(m=>m.machine_id===item.source_machine_id)?.name??item.source_machine_id}</a>}
          <div className="actions"><Button variant="outline" size="sm" disabled={item.state !== "ready" || ((item.capture_mode ?? item.capture_summary?.capture_mode) !== "vm" && item.task_content !== "clean")} onClick={()=>modal("Restore",item)}>Restore</Button>
          <DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" size="icon-sm" aria-label={"Snapshot actions for "+(item.label||item.snapshot_id)}><Ellipsis/></Button></DropdownMenuTrigger>
          <DropdownMenuContent align="end">{((item.capture_mode ?? item.capture_summary?.capture_mode) === "vm" ? (machine ? ["Rollback"] : []) : ["Export","Verify",...(machine?["Rollback"]:[]),"Delete"]).map(mode=><DropdownMenuItem key={mode} onSelect={()=>modal(mode,item)}>{mode}</DropdownMenuItem>)}</DropdownMenuContent></DropdownMenu></div>
        </div><details className="disclosure"><summary>Artifact details</summary><Details value={{snapshot_id:item.snapshot_id,bytes:item.size_bytes,sha256:item.sha256,provenance:item.provenance,vm_provenance:item.vm_provenance,closure_verified:item.closure_verified_at,restore_verified:item.restore_verified_at}}/></details>
        <CaptureDetails item={item} />
      </div>)}</div>
  if (directory) return <><Callout error={error} />{rows}</>
  if (machine && captures && !captures.length && !history.error) {
    const vm = bundleBound(machine)
    const blocked = vm ? vmBlock : cleanBlock && recoveryBlock
    return <WorkspaceGuide label="No checkpoints" title="Save this machine’s state"
      description="Capture the supported Guest state and restore it at a later point."
      actions={<Button disabled={Boolean(blocked)} title={blocked} onClick={() => captureMachine(machine, actions, vm)}>Checkpoint</Button>}
      commands={["# Save this machine's state", "sandboxctl snapshots capture " + machine.machine_id + (vm ? " --capture-mode vm" : ""),
        "", "# List its checkpoints", "sandboxctl snapshots list " + machine.machine_id]} />
  }
  return <>
    <Section title={machine ? "Checkpoints" : "Snapshot history"} actions={<>
      <button onClick={() => actions.dialog({
        title: "Import snapshot", fields: [{ name: "archive", label: "Snapshot archive", type: "file", required: true }],
        act: async form => {
          const file = form.get("archive") as File
          const digest = await fileDigest(file)
          return actions.tracked(intent => api.upload("snapshots/import", file, { method: "POST", sha256: digest, intent }))
        }
      })}>Import</button>
      {machine && <button className="primary" disabled={Boolean(cleanBlock && recoveryBlock)} onClick={() => captureMachine(machine, actions, false)}>Capture</button>}
      {machine && bundleBound(machine) && <button disabled={Boolean(vmBlock)} onClick={() => captureMachine(machine, actions, true)}>Capture VM checkpoint</button>}</>}>
      <FeatureStatus reason={cleanBlock && "Clean checkpoint · " + cleanBlock} />
      <FeatureStatus reason={recoveryBlock && "Active recovery · " + recoveryBlock} />
      {machine && bundleBound(machine) && <FeatureStatus reason={vmBlock && "VM checkpoint · " + vmBlock} />}
      <Callout error={history.error} /><Callout error={error} />
      {!captures?.length && !history.error && <p className="empty" role="status">{captures ? "No snapshots yet." : "Loading checkpoints…"}</p>}
      {rows}
    </Section>
  </>
}

function CaptureDetails({ item }: { item: Snapshot }) {
  const summary = item.capture_summary
  const config = summary?.configuration
  const spec = config?.status === "verified" ? config.spec : undefined
  const resources = spec?.resources
  return <details className="disclosure">
    <summary>Captured configuration</summary>
    <Details value={{
      configuration: config?.status === "verified" ? "Verified" : "Unknown · capture record unavailable",
      source_revision: config?.source_machine_revision ?? "Unknown",
      CPU: resources?.cpu_count ?? "Unknown", memory_MiB: resources?.memory_mib ?? "Unknown",
      guest_storage_GiB: resources?.storage_gib ?? "Unknown", boot_mode: spec?.boot_mode ?? "Unknown",
      template: spec?.template_id ?? config?.template_id ?? "Unknown",
      RAM: summary?.ram ? label(summary.ram) : "Unknown", RAM_bytes: summary?.ram_bytes ?? "Unknown",
      disks: summary?.disks ?? "Unknown", restore_policies: summary?.restore_policies?.map(label).join(", ") || "Unknown",
      source: config?.source, specification_digest: config?.spec_digest,
      manifest_digest: config?.manifest_digest, verified: summary?.verified_at ? time(summary.verified_at) : undefined,
      reason: config?.reason_code ?? summary?.reason_code,
    }} />
  </details>
}


function leaseDescription(lease: RunningLease) {
  if (lease.state === "suspect") {
    return lease.release_reason === "host boot changed"
      ? "The host restarted. This lease still protects the machine."
      : "Renewal is overdue. This lease still protects the machine."
  }
  if (lease.state === "released") {
    return lease.release_reason === "expired" ? "The lease expired and its protection ended." : "Lease protection ended."
  }
  return lease.semantics_version === 2 ? "Protection ends at expiry." : "Protection continues until release."
}

function activityBlocker(blocker: string) {
  const parts = blocker.split(":")
  if (!parts.includes("unknown")) return blocker
  const source = parts.filter(part => part !== "unknown").join(" / ").replaceAll("_", " ")
  return `Automatic sleep is blocked while ${source || "activity"} status is unknown.`
}

function Activity({ machine, actions, revision, policyOnly = false }: Context & { policyOnly?: boolean }) {
  const policy = machine.lifecycle_policy
  const blockers = policy?.effective_blockers ?? policy?.worker?.blockers ?? machine.blocked_reasons
  const base = machinePath(machine.machine_id)
  const leases = useCollection<RunningLease>(policyOnly ? "" : base + "/leases", revision)
  const [newLease, setNewLease] = useState<Record<string, unknown>>()
  const changeLease = (lease: RunningLease, release: boolean) => actions.dialog({
    title: release ? "Release lease" : "Renew lease",
    fields: [
      { name: "lease_token", label: "Lease token", type: "password", required: true },
      ...(release ? [] : [{ name: "ttl_seconds", label: "Duration (seconds)", type: "number" as const, min: 1, value: 300 }])
    ],
    act: form => actions.mutate("leases/" + segment(lease.lease_id), {
      lease_token: str(form, "lease_token"), ...(release ? {} : { ttl_seconds: num(form, "ttl_seconds") })
    }, { method: release ? "DELETE" : "PATCH", revision: lease.revision })
  })
  return <>
    {policy?.supported ? <Section title="Lifecycle policy" actions={<button disabled={policy.desired_revision !== policy.applied_revision} onClick={() => actions.dialog({
      title: "Edit lifecycle policy", submit: "Apply", sheet: true, fields: policyFields(policy.desired),
      act: form => {
        const overrides = policyOverrides(form, policy.desired)
        if (!Object.keys(overrides).length) throw new Error("Choose a policy setting to change.")
        return actions.mutate(base, { expected_generation: machine.binding_generation,
          expected_policy_revision: policy.desired_revision, lifecycle_policy: overrides },
          { method: "PATCH", revision: machine.revision })
      },
    })}>Configure</button>}>
      <div className={styles.policyStatus}>
        <State value={policy.effective_status ?? "unknown"} />
        <span>{policy.source === "default" ? "Inherited from platform defaults" : policy.source === "override" ? "Machine override" : label(policy.source)}</span>
        {policy.operation_id && <a href={"#/operations/" + segment(policy.operation_id)}>View policy operation</a>}
      </div>
      {policy.desired_revision !== policy.applied_revision && <p className={styles.note}>The requested policy is revision {policy.desired_revision}. The active policy is revision {policy.applied_revision}.</p>}
      <Details value={{ source: policy.source, default_revision: policy.default_revision, desired_revision: policy.desired_revision,
        applied_revision: policy.applied_revision, effective_status: policy.effective_status, keep_running: policy.applied?.keep_running, standby_after_seconds: policy.applied?.standby_after_seconds,
        hibernate_after_seconds: policy.applied?.hibernate_after_seconds, idle_seconds: policy.worker?.idle_ms == null ? undefined : Math.floor(policy.worker.idle_ms / 1000),
        overrides: policy.overrides, blockers: blockers?.map(activityBlocker), worker_power: policy.worker?.power,
        next_action: policy.worker?.next_action?.kind, next_action_origin: policy.worker?.next_action?.origin,
        eligible_after_seconds: policy.worker?.next_action && Math.ceil(policy.worker.next_action.eligible_after_ms / 1000),
        last_decision: policy.worker?.last_decision, last_error: policy.last_error }} />
      {policy.worker && <details className="disclosure"><summary>Worker activity observations</summary>
        <Details value={{
          worker_power: policy.worker.power, execution_epoch: policy.worker.execution_epoch,
          collector_generation: policy.worker.collector_generation,
          activity: Object.fromEntries(Object.entries(policy.worker.collectors ?? {}).map(([key, value]) =>
            [key, value.fresh === undefined ? "Unknown" : value.fresh ? "Current" : "Stale"])),
          pressure_admission: policy.worker.pressure?.admission_throttled ? "Throttled" : undefined,
        }} />
      </details>}
    </Section> : bundleBound(machine) ? <Section title="Lifecycle policy">
      <p className={styles.note}>Lifecycle policy support has not been verified for this machine.</p>
    </Section> : <Section title="Idle policy" actions={<button onClick={() => actions.dialog({
      title: "Configure idle policy", submit: "Save",
      fields: [{ name: "enabled", label: "Automatically pause when idle", type: "checkbox", value: machine.idle_policy?.enabled },
        { name: "keep_running", label: "Keep running", type: "checkbox", value: machine.keep_running },
        { name: "timeout_seconds", label: "Idle timeout (seconds)", type: "number", min: 1, value: machine.idle_policy?.timeout_seconds, required: true }],
      act: form => actions.mutate(base, {
        idle_policy: { enabled: form.has("enabled"), timeout_seconds: num(form, "timeout_seconds") }, keep_running: form.has("keep_running"), expected_generation: machine.binding_generation
      }, { method: "PATCH", revision: machine.revision })
    })}>Configure</button>}>
      <p className="policy-note">Authorized access can wake a hibernated machine when its policy permits.</p>
      <FeatureStatus reason={capabilityReason(machine, "automatic_idle")} />
      <Details value={{ enabled: machine.idle_policy?.enabled, timeout_seconds: machine.idle_policy?.timeout_seconds, keep_running: machine.keep_running,
        effective_status: policy?.effective_status, blockers: blockers?.map(activityBlocker), last_activity: time(machine.last_user_activity_at) }} />
    </Section>}
    {!policyOnly && <Section title="Leases" actions={<button onClick={() => actions.dialog({
      title: "Acquire keep-running lease",
      fields: [{ name: "reason", label: "Reason", required: true }, { name: "ttl_seconds", label: "Duration (seconds)", type: "number", min: 1, value: 300 }],
      act: async form => setNewLease(await actions.mutate<Record<string, unknown>>(base + "/leases", {
        reason: str(form, "reason"), ttl_seconds: num(form, "ttl_seconds"), ...(policy?.supported ? { semantics_version: 2 } : {})
      }, { revision: machine.revision }))
    })}>Acquire lease</button>}>
      <Callout error={leases.error} />
      {newLease && <Details value={newLease} />}
      <div className="rows">{leases.items?.map(lease => <div className="row" key={lease.lease_id}>
        <code>{lease.lease_id}</code><State value={lease.state === "released" && lease.release_reason === "expired" ? "expired" : lease.state} /><span>{lease.reason}</span><span>{time(lease.expires_at)}</span><small>{leaseDescription(lease)}</small>
        <button disabled={lease.state === "released"} onClick={() => changeLease(lease, false)}>Renew</button><button disabled={lease.state === "released"} onClick={() => changeLease(lease, true)}>Release</button>
      </div>)}</div>
    </Section>}
  </>
}
