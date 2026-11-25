import { lazy, Suspense, useEffect, useRef, useState } from "react"
import type { Machine, Page, Service, Snapshot, Task } from "../../sdk/typescript/src/index"
import { machine as machinePath, service, snapshot, task } from "../../sdk/typescript/src/index"
import type { Actions } from "./main"
import { api, argv, Callout, Details, download, label, num, query, Records, Section, segment, State, str, time, useRemote } from "./common"
const Terminal = lazy(() => import("./Terminal"))

type Context = { machine: Machine; actions: Actions; revision: number }
const tabs = ["overview", "terminal", "tasks", "files", "services", "branches", "snapshots", "activity"]
const commandFields = [
  { name: "argv", label: "Command arguments (JSON array)", type: "textarea" as const, value: '["/bin/sh"]', required: true },
  { name: "cwd", label: "Guest working directory", value: "/workspace" },
  { name: "user", label: "Guest user", value: "root" }
]
const command = (form: FormData) => ({ argv: argv(form), cwd: str(form, "cwd"), user: str(form, "user") })
const taskID = (value: Task) => value.task_id ?? value.id ?? ""

export function MachineView({ tab, ...context }: Context & { tab: string }) {
  const { machine, actions } = context
  const base = machinePath(machine.machine_id)
  const path = "#/" + base
  const confirmPower = (action: string) => actions.dialog({
    title: `${label(action)} machine`,
    description: action === "stop" ? "Stopping discards in-memory execution state. Guest disk contents remain available." : undefined,
    destructive: action === "stop" || action === "delete",
    act: () => action === "delete"
      ? actions.mutate(base, { expected_generation: machine.binding_generation }, { method: "DELETE", revision: machine.revision })
      : actions.mutate(base + "/" + action, { expected_generation: machine.binding_generation }, { revision: machine.revision })
  })
  return <>
    <a className="breadcrumb" href="#/machines">‹ All machines</a>
    <header className="page-head"><div><h1>{machine.name}</h1><p className="machine-id"><code>{machine.machine_id}</code><State value={machine.observed_state} /><State value={machine.availability} /></p></div>
      <div className="actions">{["start", "stop", "pause", "resume", "wake"].map(action =>
        <button key={action} onClick={() => confirmPower(action)}>{label(action)}</button>
      )}<button onClick={() => confirmPower("delete")} className="quiet-danger">Delete</button></div></header>
    <nav className="tabs" aria-label="Machine sections">{tabs.map(item => <a className={tab === item ? "active" : ""} href={path + "/" + item} key={item}>{label(item)}</a>)}</nav>
    {machine.blocked_reasons?.length ? <div className="blockers">{machine.blocked_reasons.map(reason => <span key={reason}>{label(reason)}</span>)}</div> : null}
    {tab === "terminal" ? <Execs {...context} />
      : tab === "tasks" ? <Tasks {...context} />
        : tab === "files" ? <Files {...context} />
          : tab === "services" ? <Services {...context} />
            : tab === "branches" ? <Branches {...context} />
              : tab === "snapshots" ? <Snapshots {...context} />
                : tab === "activity" ? <Activity {...context} />
                  : <Overview {...context} />}
  </>
}

function Overview({ machine, actions }: Context) {
  const resources = machine.resources ?? machine.spec?.resources
  return <>
    <Section title="Machine configuration" actions={<button onClick={() => actions.dialog({
      title: "Edit machine", submit: "Save",
      fields: [
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
      <Details value={{
        template: machine.template_id, desired_power: machine.desired_power,
        binding_generation: machine.binding_generation, revision: machine.revision,
        runtime_name: machine.runtime_name, runtime_identity: machine.runtime_identity,
        last_observed: time(machine.observed_at), last_activity: time(machine.last_user_activity_at),
        guest_management: machine.guest_management_status, vCPUs: resources?.cpu_count,
        memory_MiB: resources?.memory_mib, storage_GiB: resources?.storage_gib,
        overlay_GiB: resources?.overlay_gib
      }} />
      {machine.operation_id && <div className="panel-footer"><a href={"#/operations/" + segment(machine.operation_id)}>Operation {machine.operation_id}</a></div>}
    </Section>
    {machine.capabilities && <Section title="Capabilities">
      <div className="rows">{Object.entries(machine.capabilities).map(([key, capability]) =>
        <div className="row" key={key}><strong>{label(key)}</strong><State value={capability.supported ? "enabled" : "unavailable"} />
          <span className="muted">{capability.message ?? capability.reason_code}</span></div>
      )}</div>
    </Section>}
  </>
}

type Exec = { exec_id: string; state?: string; revision?: number; exit_code?: number | null; tty?: boolean }
type ExecResult = Exec & { output?: { stream: string; data: string }[]; next?: number; gap?: unknown; error?: string }
function Execs({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id) + "/execs"
  const sessions = useRemote<Page<Exec>>(path, revision)
  const [current, setCurrent] = useState<string>()
  const [result, setResult] = useState<ExecResult>()
  const open = () => actions.dialog({
    title: "Open terminal", fields: commandFields,
    act: async form => {
      const response = await actions.mutate<Exec>(path, {
        ...command(form), tty: true, interactive: true, cols: 100, rows: 28
      }, { revision: machine.revision })
      if (response?.exec_id) setCurrent(response.exec_id)
    }
  })
  const run = () => actions.dialog({
    title: "Run command", fields: [...commandFields, { name: "wait_seconds", label: "Wait (seconds)", type: "number", value: 5, min: 0 }],
    act: async form => setResult(await actions.mutate<ExecResult>(machinePath(machine.machine_id) + "/exec", {
      ...command(form), wait_seconds: num(form, "wait_seconds")
    }, { revision: machine.revision }))
  })
  return <>
    <Section title="Native sessions" actions={<><button onClick={run}>Run command</button><button className="primary" onClick={open}>Open terminal</button></>}>
      <Callout error={sessions.error} />
      {(sessions.data?.items ?? []).length === 0 ? <p className="empty">No sessions returned.</p> : <div className="rows">
        {sessions.data?.items.map(exec => <div className="row" key={exec.exec_id}>
          <code>{exec.exec_id}</code><State value={exec.state} /><span>{exec.exit_code == null ? "" : `Exit ${exec.exit_code}`}</span>
          <button onClick={() => setCurrent(exec.exec_id)}>Attach</button>
          <button className="quiet-danger" onClick={() => actions.dialog({
            title: "Terminate session", destructive: true,
            act: () => actions.mutate("execs/" + segment(exec.exec_id), {}, { method: "DELETE", revision: exec.revision })
          })}>Terminate</button>
        </div>)}
      </div>}
    </Section>
    {current && <Section title="Terminal" actions={<button onClick={() => setCurrent(undefined)}>Disconnect</button>}><Suspense fallback={<p className="empty">Loading terminal…</p>}><Terminal key={current} execId={current} /></Suspense></Section>}
    {result && <Section title="Command response">
      <Details value={{ exec: result.exec_id, state: result.state, exit_code: result.exit_code, output_cursor: result.next, output_gap: result.gap, error: result.error }} />
      <pre className="record">{result.output?.map(entry => {
        const bytes = Uint8Array.from(atob(entry.data), char => char.charCodeAt(0))
        return new TextDecoder().decode(bytes)
      }).join("")}</pre>
    </Section>}
  </>
}

function Tasks({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id) + "/tasks"
  const history = useRemote<Page<Task>>(path, revision)
  const [selected, setSelected] = useState<string>()
  const [stream, setStream] = useState("stdout")
  const detail = useRemote<Task>(selected ? task(selected) : "", revision)
  return <>
    <Section title="Persistent tasks" actions={<button className="primary" onClick={() => actions.dialog({
      title: "Start task", fields: [...commandFields, { name: "task_key", label: "Task key (optional)" }],
      act: form => actions.mutate(path, { ...command(form), expected_generation: machine.binding_generation, ...(str(form, "task_key") ? { task_key: str(form, "task_key") } : {}) }, { revision: machine.revision })
    })}>Start task</button>}>
      <Callout error={history.error} />
      {history.data?.items.length ? <div className="rows">{history.data.items.map(item => <div className="row" key={taskID(item)}>
        <button className="text-button" onClick={() => setSelected(taskID(item))}>{taskID(item)}</button>
        <State value={item.state} /><span>{item.exit_code == null ? "" : `Exit ${item.exit_code}`}</span><span className="muted">{time(item.observed_at)}</span>
        {["accepted", "starting", "running", "cancelling"].includes(item.state) && <button onClick={() => actions.dialog({
          title: "Cancel task", destructive: true, fields: [{ name: "grace_seconds", label: "Grace period (seconds)", type: "number", min: 0, value: 5 }],
          act: form => actions.mutate(task(taskID(item)) + "/cancel", { grace_seconds: num(form, "grace_seconds") }, { revision: machine.revision })
        })}>Cancel</button>}
      </div>)}</div> : <p className="empty">No tasks returned.</p>}
    </Section>
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

type FileItem = { name: string; path?: string; is_dir?: boolean; directory?: boolean; type?: string; size?: number; size_bytes?: number; mode?: string | number }
function Files({ machine, actions, revision }: Context) {
  const [path, setPath] = useState("/workspace")
  const [input, setInput] = useState(path)
  const base = machinePath(machine.machine_id) + "/files"
  const list = useRemote<{ items?: FileItem[]; entries?: FileItem[] }>(base + "/list?" + query(path), revision)
  const [error, setError] = useState<unknown>()
  const open = (target: string) => { setInput(target); setPath(target); setError(undefined) }
  const edit = async (target: string) => {
    try {
      const response = await api.response(base + "?" + query(target))
      const blob = await response.blob()
      if (blob.size > 1048576) throw new Error("Text editing is limited to 1 MiB.")
      const content = new TextDecoder("utf-8", { fatal: true }).decode(await blob.arrayBuffer())
      actions.dialog({
        title: "Edit Guest file", description: target, submit: "Save",
        fields: [{ name: "content", label: "File contents", type: "textarea", value: content }],
        act: form => actions.tracked(intent => api.upload(base + "?" + query(target), new Blob([str(form, "content")]), { intent, revision: machine.revision }))
      })
    } catch (error) { setError(error) }
  }
  return <Section title="Guest files" actions={<button onClick={() => actions.dialog({
    title: "Upload Guest file",
    fields: [
      { name: "target", label: "Destination Guest path, including file name", value: path.replace(/\/$/, "") + "/", required: true },
      { name: "file", label: "Local file", type: "file", required: true }
    ],
    act: form => actions.tracked(intent => api.upload(base + "?" + query(str(form, "target")), form.get("file") as File, { intent, revision: machine.revision }))
  })}>Upload</button>}>
    <Callout error={list.error} /><Callout error={error} />
    <form className="inline-form" onSubmit={event => { event.preventDefault(); open(input) }}>
      <label htmlFor="file-path">Guest directory</label><input id="file-path" value={input} onChange={event => setInput(event.target.value)} required />
      <button>Open</button>{path !== "/" && <button type="button" onClick={() => open(path.replace(/\/?[^/]+\/?$/, "") || "/")}>Parent</button>}
    </form>
    <div className="rows">{(list.data?.items ?? list.data?.entries ?? []).map(item => {
      const full = item.path ?? path.replace(/\/$/, "") + "/" + item.name
      const directory = item.is_dir || item.directory || item.type === "directory" || item.type === "dir"
      return <div className="row" key={full}>
        {directory ? <button className="text-button" onClick={() => open(full)}>{item.name}/</button> : <strong>{item.name}</strong>}
        <span>{item.size_bytes ?? item.size ?? "—"} bytes</span><span>{item.mode ?? "—"}</span>
        {!directory && <><button onClick={() => void edit(full)}>Edit text</button>
          <button onClick={() => api.response(base + "?" + query(full)).then(response => download(response, item.name)).catch(setError)}>Download</button></>}
      </div>
    })}</div>
  </Section>
}

function Services({ machine, actions, revision }: Context) {
  const path = machinePath(machine.machine_id) + "/services"
  const list = useRemote<Page<Service>>(path, revision)
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
      { name: "protocol", label: "Protocol", value: current?.protocol ?? "http", options: ["http", "https", "ssh", "tcp-mtls"].map(value => ({ value })) },
      { name: "guest_port", label: "Guest port", type: "number", value: current?.guest_port ?? 8080, min: 1, required: true },
      { name: "readiness", label: "Readiness settings (JSON)", type: "textarea", value: current?.readiness ? JSON.stringify(current.readiness) : "" },
      { name: "auth", label: "Access settings (JSON)", type: "textarea", value: current?.auth ? JSON.stringify(current.auth) : "" }
    ],
    act: form => actions.mutate(current ? service(current.service_id) : path, {
      name: str(form, "name"), protocol: str(form, "protocol"), guest_port: num(form, "guest_port"),
      ...(current ? {} : { expected_generation: machine.binding_generation }),
      ...(str(form, "readiness") ? { readiness: JSON.parse(str(form, "readiness")) } : {}),
      ...(str(form, "auth") ? { auth: JSON.parse(str(form, "auth")) } : {})
    }, { method: current ? "PATCH" : "POST", revision: current?.revision ?? machine.revision })
  })
  return <Section title="Services" actions={<button className="primary" onClick={() => configure()}>Create service</button>}>
    <Callout error={list.error} /><Callout error={error} />
    {!list.data?.items.length && <p className="empty">No services returned.</p>}
    <div className="rows">{list.data?.items.map(item => {
      const ssh = item.protocol === "ssh"
      const http = item.protocol === "http" || item.protocol === "https"
      const url = item.public_host && http ? `https://${item.public_host}${item.public_port ? ":" + item.public_port : ""}` : undefined
      const connect = ssh ? `sandboxctl ssh-config ${machine.machine_id}` : url
        ? `curl -H "Authorization: Bearer \$SANDBOX_TOKEN" "${url}"`
        : `sandboxctl port-forward ${item.service_id} --local-port 8080`
      return <div className="service-row" key={item.service_id}>
        <div className="row"><strong>{item.name}</strong><code>{item.protocol} · :{item.guest_port}</code>
          <State value={typeof item.status === "string" ? item.status : typeof item.readiness === "string" ? item.readiness : undefined} />
          {url && <button onClick={() => void preview(item)}>Open preview ↗</button>}
          <button onClick={() => navigator.clipboard.writeText(connect).catch(setError)}>Copy connection command</button>
          <button onClick={() => configure(item)}>Edit</button><button className="quiet-danger" onClick={() => actions.dialog({
            title: "Delete service", destructive: true,
            act: () => actions.mutate(service(item.service_id), {}, { method: "DELETE", revision: item.revision })
          })}>Delete</button>
        </div>
        <Details value={{ host: item.public_host, public_port: item.public_port, readiness: item.readiness, auth: item.auth, ssh_fingerprint: item.ssh_fingerprint, connection: connect }} />
      </div>
    })}</div>
  </Section>
}

type Dependency = { parent_machine_id: string; child_machine_id: string; kind: string; status: string; observed_at?: string }
function Branches({ machine, actions, revision }: Context) {
  const base = machinePath(machine.machine_id)
  const dependencies = useRemote<{ items: Dependency[] }>(base + "/dependencies", revision)
  const copy = (kind: "forks" | "clones") => actions.dialog({
    title: kind === "forks" ? "Live Fork" : "Independent Clone",
    fields: [{ name: "child_name", label: "Child machine name", required: true }],
    act: form => actions.mutate(base + "/" + kind, {
      child_name: str(form, "child_name"), expected_generation: machine.binding_generation,
      ...(kind === "clones" ? { copy_policy: "managed_clean" } : {})
    }, { revision: machine.revision })
  })
  const machineLink = (id: string) => id === machine.machine_id
    ? <strong>{machine.name}</strong>
    : <a href={"#/" + machinePath(id) + "/branches"}>{id}</a>
  return <Section title="Lineage and runtime dependencies" actions={<><button onClick={() => copy("forks")}>Live Fork</button><button onClick={() => copy("clones")}>Independent Clone</button></>}>
    <Callout error={dependencies.error} />
    {dependencies.data?.items.length ? <div className="rows">{dependencies.data.items.map(item =>
      <div className="row" key={[item.parent_machine_id, item.child_machine_id, item.kind].join("/")}>
        <span>{machineLink(item.parent_machine_id)} <span aria-label="to">→</span> {machineLink(item.child_machine_id)}</span>
        <strong>{label(item.kind)}</strong><State value={item.status} /><span className="muted">{time(item.observed_at)}</span>
      </div>
    )}</div> : <p className="empty">{dependencies.data ? "No dependencies recorded." : "Loading dependencies…"}</p>}
  </Section>
}

function Snapshots({ machine, actions, revision }: Context) {
  const base = machinePath(machine.machine_id)
  const history = useRemote<Page<Snapshot>>(base + "/snapshots", revision)
  const [error, setError] = useState<unknown>()
  const modal = (mode: string, item: Snapshot) => {
    const resource = snapshot(item.snapshot_id)
    if (mode === "Export") {
      api.response(resource + "/export").then(response => download(response, item.snapshot_id + ".tar")).catch(setError)
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
        if (mode === "Rollback") return actions.mutate(base + "/rollback", {
          snapshot_id: item.snapshot_id, expected_generation: machine.binding_generation,
          old_state_policy: "export_then_delete_on_paused", task_disposition: str(form, "task_disposition")
        }, { revision: machine.revision })
        if (mode === "Restore") return actions.mutate("restores", { snapshot_id: item.snapshot_id, name: str(form, "name") })
        if (mode === "Verify") return actions.mutate(resource + "/verify", { verification: str(form, "verification") }, { revision: item.revision })
        return actions.mutate(resource, {}, { method: "DELETE", revision: item.revision })
      }
    })
  }
  return <>
    <Section title="Snapshot history" actions={<>
      <button onClick={() => actions.dialog({
        title: "Import snapshot", fields: [{ name: "archive", label: "Snapshot archive", type: "file", required: true }],
        act: async form => {
          const file = form.get("archive") as File
          const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", await file.arrayBuffer()))
          const sha256 = Array.from(hash, value => value.toString(16).padStart(2, "0")).join("")
          return actions.tracked(intent => api.upload("snapshots/import", file, { method: "POST", sha256, intent }))
        }
      })}>Import</button>
      <button className="primary" onClick={() => actions.dialog({
        title: "Capture snapshot",
        fields: [{ name: "label", label: "Label" }, { name: "purpose", label: "Purpose", options: [{ value: "clone" }, { value: "recovery" }] }],
        act: form => actions.mutate(base + "/snapshots", { label: str(form, "label"), purpose: str(form, "purpose"), expected_generation: machine.binding_generation }, { revision: machine.revision })
      })}>Capture</button></>}>
      <Callout error={history.error} /><Callout error={error} />
      {!history.data?.items.length && <p className="empty">No snapshots returned.</p>}
      <div className="rows">{history.data?.items.map(item => <div key={item.snapshot_id}>
        <div className="row"><strong>{item.label || item.snapshot_id}</strong><State value={item.state} />
          <code>{item.purpose} · {item.task_content}</code>
          {["Export", "Verify", "Restore", "Rollback", "Delete"].map(mode => <button key={mode} className={mode === "Delete" ? "quiet-danger" : ""} onClick={() => modal(mode, item)}>{mode}</button>)}
        </div><Details value={{ snapshot_id: item.snapshot_id, created: time(item.created_at), bytes: item.size_bytes, sha256: item.sha256, provenance: item.provenance, closure_verified: item.closure_verified_at, restore_verified: item.restore_verified_at }} />
      </div>)}</div>
    </Section>
  </>
}

type Lease = { lease_id: string; state: string; reason?: string; expires_at?: string; revision?: number }
function Activity({ machine, actions, revision }: Context) {
  const base = machinePath(machine.machine_id)
  const leases = useRemote<Page<Lease>>(base + "/leases", revision)
  const metrics = useRemote<Record<string, unknown>>(base + "/metrics", revision)
  const [newLease, setNewLease] = useState<Record<string, unknown>>()
  const changeLease = (lease: Lease, release: boolean) => actions.dialog({
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
    <Section title="Idle policy" actions={<button onClick={() => actions.dialog({
      title: "Configure idle policy", submit: "Save",
      fields: [{ name: "enabled", label: "Automatically pause when idle", type: "checkbox", value: machine.idle_policy?.enabled },
        { name: "keep_running", label: "Keep running", type: "checkbox", value: machine.keep_running },
        { name: "timeout_seconds", label: "Idle timeout (seconds)", type: "number", min: 1, value: machine.idle_policy?.timeout_seconds, required: true }],
      act: form => actions.mutate(base, {
        idle_policy: { enabled: form.has("enabled"), timeout_seconds: num(form, "timeout_seconds") }, keep_running: form.has("keep_running"), expected_generation: machine.binding_generation
      }, { method: "PATCH", revision: machine.revision })
    })}>Configure</button>}>
      <Details value={{ enabled: machine.idle_policy?.enabled, timeout_seconds: machine.idle_policy?.timeout_seconds, keep_running: machine.keep_running, blockers: machine.blocked_reasons, last_activity: time(machine.last_user_activity_at) }} />
    </Section>
    <Section title="Leases" actions={<button onClick={() => actions.dialog({
      title: "Acquire keep-running lease",
      fields: [{ name: "reason", label: "Reason", required: true }, { name: "ttl_seconds", label: "Duration (seconds)", type: "number", min: 1, value: 300 }],
      act: async form => setNewLease(await actions.mutate<Record<string, unknown>>(base + "/leases", {
        reason: str(form, "reason"), ttl_seconds: num(form, "ttl_seconds")
      }, { revision: machine.revision }))
    })}>Acquire lease</button>}>
      <Callout error={leases.error} />
      {newLease && <Details value={newLease} />}
      <div className="rows">{leases.data?.items.map(lease => <div className="row" key={lease.lease_id}>
        <code>{lease.lease_id}</code><State value={lease.state} /><span>{lease.reason}</span><span>{time(lease.expires_at)}</span>
        <button onClick={() => changeLease(lease, false)}>Renew</button><button onClick={() => changeLease(lease, true)}>Release</button>
      </div>)}</div>
    </Section>
    <Section title="Resource metrics" actions={<button onClick={actions.refresh}>Refresh</button>}>
      <Callout error={metrics.error} />{metrics.data && <Details value={metrics.data} />}
    </Section>
  </>
}
