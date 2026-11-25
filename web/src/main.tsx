import { FormEvent, useEffect, useRef, useState } from "react"
import { createRoot } from "react-dom/client"
import { Control, Machine, Mutation, Operation, Page, PlatformError, terminalStates } from "../../sdk/typescript/src/index"
import { api, Callout, Details, Dialog, DialogConfig, isAuthError, label, num, Records, Section, segment, State, str, time, useRemote } from "./common"
import { MachineView } from "./machine"
import { System, SystemTab } from "./system"
import "./style.css"

const operationStorage = "sandbox.operations"
const recent = () => {
  try { return JSON.parse(sessionStorage.getItem(operationStorage) ?? "[]") as string[] } catch { return [] }
}

export type Actions = {
  dialog: (config: DialogConfig) => void
  mutate: <T = Operation>(path: string, body?: unknown, options?: { revision?: number; method?: string }) => Promise<T>
  tracked: <T>(action: (intent: Mutation) => Promise<T>) => Promise<T>
  refresh: () => void
}

type Location = { page: "machines" | "machine" | SystemTab; id?: string; tab?: string }
function route(): Location {
  const [, head, id, tab] = location.hash.split("/")
  if (head === "machines" && id) {
    try { return { page: "machine", id: decodeURIComponent(id), tab: tab ?? "overview" } } catch { return { page: "machines" } }
  }
  if (head === "operations" || head === "settings" || head === "diagnostics" || head === "templates") return { page: head, id: id || undefined }
  return { page: "machines" }
}

function Logo() {
  return <div className="brand"><div className="logo" aria-hidden><i /><i /><i /></div><div>Sandbox<span>Console</span></div></div>
}

function Login({ ready }: { ready: () => void }) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    setBusy(true)
    setError(undefined)
    try { await api.login(str(form, "username"), str(form, "password")); ready() } catch (error) { setError(error) }
    finally { setBusy(false) }
  }
  return <main className="login"><section><Logo /><h1>Sign in</h1><form onSubmit={submit}>
    <label className="field"><span>Username</span><input name="username" autoComplete="username" autoFocus required /></label>
    <label className="field"><span>Password</span><input name="password" type="password" autoComplete="current-password" required /></label>
    <Callout error={error} /><button className="primary" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
  </form></section></main>
}

function App() {
  const [at, setAt] = useState(route)
  const [auth, setAuth] = useState<"checking" | "signed_in" | "signed_out">("checking")
  const [sessionError, setSessionError] = useState<unknown>()
  const [revision, revisionSet] = useState(0)
  const [control, setControl] = useState<Control>()
  const [config, setDialog] = useState<DialogConfig>()
  const [notice, setNotice] = useState<Operation>()
  const [ops, setOps] = useState<string[]>(recent)
  const [streamError, setStreamError] = useState<unknown>()
  const streamID = useRef("")
  const retry = useRef<{ key: string; intent: Mutation } | undefined>(undefined)
  const refresh = () => revisionSet(value => value + 1)
  useEffect(() => {
    const update = () => setAt(route())
    window.addEventListener("hashchange", update)
    return () => window.removeEventListener("hashchange", update)
  }, [])
  useEffect(() => {
    if (auth !== "checking") return
    const abort = new AbortController()
    api.control(abort.signal).then(result => {
      setControl(result)
      setAuth("signed_in")
      setSessionError(undefined)
    }).catch(error => {
      if (abort.signal.aborted) return
      if (isAuthError(error)) setAuth("signed_out")
      else setSessionError(error)
    })
    return () => abort.abort()
  }, [auth])

  useEffect(() => {
    if (auth !== "signed_in") return
    const abort = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    const read = async () => {
      try {
        for await (const event of api.events(streamID.current, abort.signal)) {
          if (event.id) streamID.current = event.id
          setStreamError(undefined)
          refresh()
        }
      } catch (error) {
        if (abort.signal.aborted) return
        if (isAuthError(error)) { setAuth("signed_out"); return }
        if (error instanceof PlatformError && [409, 410].includes(error.status)) {
          streamID.current = ""
          refresh()
        } else {
          setStreamError(error)
        }
      }
      if (!abort.signal.aborted) timer = setTimeout(read, 2000)
    }
    void read()
    return () => { abort.abort(); clearTimeout(timer) }
  }, [auth])

  useEffect(() => {
    if (auth !== "signed_in") return
    const abort = new AbortController()
    api.control(abort.signal).then(setControl).catch(() => {})
    return () => abort.abort()
  }, [auth])

  const machines = useRemote<Page<Machine>>(auth === "signed_in" && at.page === "machines" ? "machines?limit=100" : "", revision)
  const match = useRemote<Machine>(auth === "signed_in" && at.page === "machine" && at.id ? "machines/" + segment(at.id) : "", revision)
  useEffect(() => {
    if (isAuthError(machines.error) || isAuthError(match.error)) setAuth("signed_out")
  }, [machines.error, match.error])

  const track = <T,>(result: T) => {
    if (result && typeof result === "object" && "operation_id" in result && typeof result.operation_id === "string") {
      const id = result.operation_id
      setOps(previous => {
        const next = [id, ...previous.filter(item => item !== id)].slice(0, 40)
        sessionStorage.setItem(operationStorage, JSON.stringify(next))
        return next
      })
      if ("state" in result) setNotice(result as unknown as Operation)
      void api.waitOperation(id, { onUpdate: setNotice }).then(refresh).catch(error => {
        if (isAuthError(error)) setAuth("signed_out")
        else setStreamError(error)
      })
    }
    refresh()
    return result
  }

  const actions: Actions = {
    dialog: newConfig => { retry.current = undefined; setDialog(newConfig) },
    mutate: async <T,>(path: string, body: unknown = {}, options?: { revision?: number; method?: string }) => {
      const key = JSON.stringify([path, body, options])
      const intent = retry.current?.key === key ? retry.current.intent : await api.intent()
      retry.current = { key, intent }
      try {
        const result = await api.mutate<T>(path, body, { ...options, intent })
        retry.current = undefined
        return track(result)
      } catch (error) {
        if (isAuthError(error)) setAuth("signed_out")
        throw error
      }
    },
    tracked: async action => {
      const intent = retry.current?.intent ?? await api.intent()
      retry.current = { key: "upload", intent }
      const result = await action(intent)
      retry.current = undefined
      return track(result)
    },
    refresh
  }

  if (auth === "checking") return <main className="login"><section><Logo /><p>Connecting to platform</p><Callout error={sessionError} />{sessionError ? <button onClick={() => location.reload()}>Try again</button> : null}</section></main>
  if (auth === "signed_out") return <Login ready={() => { streamID.current = ""; setAuth("checking") }} />

  return <div className="shell">
    <aside className="sidebar">
      <Logo />
      <nav aria-label="Console navigation">
        <a className={at.page === "machines" || at.page === "machine" ? "selected" : ""} href="#/machines"><span aria-hidden>▣</span>Machines</a>
        <a className={at.page === "operations" ? "selected" : ""} href="#/operations"><span aria-hidden>◴</span>Operations</a>
        <a className={at.page === "templates" ? "selected" : ""} href="#/templates"><span aria-hidden>◇</span>Templates</a>
        <a className={at.page === "diagnostics" ? "selected" : ""} href="#/diagnostics"><span aria-hidden>⌁</span>Diagnostics</a>
        <a className={at.page === "settings" ? "selected" : ""} href="#/settings"><span aria-hidden>⚙</span>Settings</a>
      </nav>
      <div className="sidebar-footer">{control?.runtime_version && <span>Runtime {control.runtime_version}</span>}<button onClick={() => {
        api.logout().then(() => {
          streamID.current = ""
          sessionStorage.removeItem(operationStorage)
          setOps([])
          setNotice(undefined)
          setAuth("signed_out")
        }).catch(setStreamError)
      }}>Sign out</button></div>
    </aside>
    <main className="workspace">
      {control?.maintenance && <div className="callout" role="status"><strong>Maintenance mode</strong></div>}
      <Callout error={streamError} />
      {notice && <div className={`operation-notice ${notice.error ? "operation-error" : ""}`} role="status">
        <State value={notice.state} /><a href={`#/operations/${segment(notice.operation_id)}`}>{label(notice.kind ?? "Operation")} · {notice.stage ?? notice.operation_id}</a>
        {notice.error?.message && <span>{notice.error.message}</span>}<button aria-label="Dismiss operation" onClick={() => setNotice(undefined)}>×</button>
      </div>}
      {at.page === "machines" && <MachineList initial={machines.data} error={machines.error} actions={actions} revision={revision} />}
      {at.page === "machine" && <><Callout error={match.error} />{match.data && <MachineView key={match.data.machine_id} machine={match.data} tab={at.tab ?? "overview"} actions={actions} revision={revision} />}</>}
      {at.page !== "machine" && at.page !== "machines" && <System key={at.page} tab={at.page} id={at.id} actions={actions} revision={revision} operationIds={ops} />}
    </main>
    {config && <Dialog config={config} close={() => { setDialog(undefined); retry.current = undefined }} />}
  </div>
}

function MachineList({ initial, error, actions, revision }: { initial?: Page<Machine>; error: unknown; actions: Actions; revision: number }) {
  const templates = useRemote<Page<{ template_id?: string; id?: string; name: string }>>("templates", revision)
  const [search, setSearch] = useState("")
  const [extra, setExtra] = useState<Machine[]>([])
  const [cursor, setCursor] = useState<string>()
  const [pageError, setPageError] = useState<unknown>()
  useEffect(() => { setExtra([]); setCursor(initial?.next_cursor) }, [initial])
  const rows = [...initial?.items ?? [], ...extra]
  const matching = rows.filter(machine => (machine.name + " " + machine.machine_id).toLowerCase().includes(search.trim().toLowerCase()))
  const create = () => actions.dialog({
    title: "Create machine", submit: "Create",
    fields: [
      { name: "name", label: "Machine name", required: true },
      { name: "template_id", label: "Template", required: true, options: [
        { value: "", label: "Select a template" }, ...(templates.data?.items ?? []).flatMap(template => {
          const id = template.template_id ?? template.id
          return id ? [{ value: id, label: template.name ?? id }] : []
        })
      ] },
      { name: "cpu", label: "vCPUs", type: "number", min: 1, value: 2, required: true },
      { name: "memory", label: "Memory (MiB)", type: "number", min: 1, value: 2048, required: true },
      { name: "storage", label: "Storage (GiB)", type: "number", min: 1, value: 20, required: true },
      { name: "overlay", label: "Overlay (GiB)", type: "number", min: 1, value: 10, required: true },
      { name: "idle", label: "Idle timeout (seconds)", type: "number", min: 60, value: 1800, required: true },
      { name: "auto_idle", label: "Automatically pause when idle", type: "checkbox", value: true },
      { name: "start", label: "Start after creation", type: "checkbox", value: true }
    ],
    act: form => actions.mutate("machines", {
      name: str(form, "name"), template_id: str(form, "template_id"),
      resources: { cpu_count: num(form, "cpu"), memory_mib: num(form, "memory"), storage_gib: num(form, "storage"), overlay_gib: num(form, "overlay") },
      idle_policy: { enabled: form.has("auto_idle"), timeout_seconds: num(form, "idle") },
      start: form.has("start")
    })
  })
  return <>
    <header className="page-head"><div><h1>Machines</h1><p>Persistent workspaces on this node</p></div><button className="primary" onClick={create} disabled={!templates.data?.items.length}>+ Create machine</button></header>
    <Callout error={error} /><Callout error={templates.error} /><Callout error={pageError} />
    <div className="stats">
      {([["Machines", rows.length], ["Running", rows.filter(row => row.observed_state === "running").length], ["Ready", rows.filter(row => row.availability === "ready").length]] as const).map(([name, value]) =>
        <div key={name}><span>{name}{initial?.next_cursor ? " on loaded pages" : ""}</span><strong>{initial ? value : "—"}</strong></div>)}
    </div>
    <Section title="Workspaces" actions={<><input aria-label="Filter machines" placeholder="Filter by name or ID" value={search} onChange={event => setSearch(event.target.value)} /><button onClick={actions.refresh}>Refresh</button></>}>
      <div className="table-wrap"><table><thead><tr><th>Name</th><th>State</th><th>Availability</th><th>Resources</th><th>Last activity</th></tr></thead><tbody>
        {matching.map(machine => {
          const resources = machine.resources ?? machine.spec?.resources
          return <tr key={machine.machine_id}><td><a href={`#/machines/${segment(machine.machine_id)}`}>{machine.name}</a><small>{machine.template_id}</small></td>
          <td><State value={machine.observed_state} /></td><td><State value={machine.availability} />{machine.blocked_reasons?.[0] && <small>{label(machine.blocked_reasons[0])}</small>}</td>
          <td>{resources ? `${resources.cpu_count} vCPU · ${resources.memory_mib} MiB` : "—"}</td>
          <td>{time(machine.last_user_activity_at)}</td></tr>
        })}
      </tbody></table></div>
      {!matching.length && <p className="empty">{initial ? search ? "No matching machines." : "No machines yet." : "Loading machines…"}</p>}
      {cursor && <div className="panel-footer"><button onClick={() => {
        api.get<Page<Machine>>("machines?" + new URLSearchParams({ limit: "100", cursor })).then(page => {
          setExtra(previous => [...previous, ...page.items]); setCursor(page.next_cursor); setPageError(undefined)
        }).catch(setPageError)
      }}>Load more</button></div>}
    </Section>
  </>
}

createRoot(document.getElementById("root")!).render(<App />)
