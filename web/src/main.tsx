import { FormEvent, useEffect, useRef, useState } from "react"
import { createRoot } from "react-dom/client"
import { Control, Machine, Mutation, Operation, PlatformError } from "../../sdk/typescript/src/index"
import { api, Callout, Dialog, DialogConfig, isAuthError, label, segment, State, str, useRemote } from "./common"
import { MachineView } from "./machine"
import { System, SystemTab } from "./system"
import { Brand, Shell, SourceLinks } from "./layout"
import { MachineList } from "./machines"
import { Catalog } from "./catalog"
import { VolumeCatalog } from "./volumes"
import { useCollection } from "./api"
import { Skeleton } from "./daytona/skeleton"
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

type Location = { page: "machines" | "machine" | "snapshots" | "volumes" | "tasks" | "networking" | SystemTab; id?: string; tab?: string }
function route(): Location {
  const [, head, id, tab] = location.hash.split("/")
  if (head === "machines" && id) {
    try { return { page: "machine", id: decodeURIComponent(id), tab: tab ?? "overview" } } catch { return { page: "machines" } }
  }
  if (head === "snapshots" || head === "volumes" || head === "tasks" || head === "networking" || head === "operations" || head === "settings" || head === "diagnostics" || head === "templates" || head === "backups") return { page: head, id: id || undefined }
  return { page: "machines" }
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
  return <main className="login"><section><Brand /><h1>Sign in</h1><form onSubmit={submit}>
    <label className="field"><span>Username</span><input name="username" autoComplete="username" autoFocus required /></label>
    <label className="field"><span>Password</span><input name="password" type="password" autoComplete="current-password" required /></label>
    <Callout error={error} /><button className="primary" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
  </form><SourceLinks/></section></main>
}

function App() {
  const [at, setAt] = useState(route)
  const [auth, setAuth] = useState<"checking" | "signed_in" | "signed_out">("checking")
  const [sessionError, setSessionError] = useState<unknown>()
  const [revision, revisionSet] = useState(0)
  const [control, setControl] = useState<Control & {runtime_ready?:boolean; principal?:{username?:string}}>()
  const [config, setDialog] = useState<DialogConfig>()
  const [notice, setNotice] = useState<Operation>()
  const [ops, setOps] = useState<string[]>(recent)
  const [streamError, setStreamError] = useState<unknown>()
  const streamID = useRef("")
  const retry = useRef<{ key: string; intent: Mutation } | undefined>(undefined)
  const refresh = () => revisionSet(value => value + 1)
  useEffect(() => {
    const update = () => {setAt(route()); window.scrollTo(0, 0)}
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
    let updateTimer: ReturnType<typeof setTimeout> | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const read = async () => {
      try {
        for await (const event of api.events(streamID.current, abort.signal)) {
          if (event.id) streamID.current = event.id
          setStreamError(undefined)
          updateTimer ??= setTimeout(() => { updateTimer = undefined; refresh() }, 240)
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
    return () => { abort.abort(); clearTimeout(timer); clearTimeout(updateTimer) }
  }, [auth])

  useEffect(() => {
    if (auth !== "signed_in") return
    const abort = new AbortController()
    api.control(abort.signal).then(setControl).catch(() => {})
    return () => abort.abort()
  }, [auth])

  const machines = useCollection<Machine>(auth === "signed_in" ? "machines" : "", revision)
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

  if (auth === "checking") return <main className="login"><section><Brand /><p>Connecting to platform</p><Callout error={sessionError} />{sessionError ? <button onClick={() => location.reload()}>Try again</button> : null}</section></main>
  if (auth === "signed_out") return <Login ready={() => { streamID.current = ""; setAuth("checking") }} />

  const catalog = at.page === "snapshots" || at.page === "tasks" || at.page === "networking" ? at.page : undefined
  return <Shell page={at.page} machine={at.page==="machine"?match.data:undefined} machines={machines.items} runtime={control?.runtime_version} ready={control?.runtime_ready} username={control?.principal?.username} signOut={()=>{
    api.logout().then(() => {
      streamID.current=""; sessionStorage.removeItem(operationStorage)
      setOps([]); setNotice(undefined); setAuth("signed_out")
    }).catch(setStreamError)
  }}>
      {control?.maintenance && <div className="callout" role="status"><strong>Maintenance mode</strong></div>}
      <Callout error={streamError} />
      {notice && <div className={`operation-notice ${notice.error ? "operation-error" : ""}`} role="status">
        <State value={notice.state} /><a href={`#/operations/${segment(notice.operation_id)}`}>{label(notice.kind ?? "Operation")} · {notice.stage ?? notice.operation_id}</a>
        {notice.effect === "partial" && <State value="partial" />}
        {notice.error?.message && <span>{notice.error.message}</span>}{notice.result?.status === "requires_restart" && <span>Resource changes require restart</span>}<button aria-label="Dismiss operation" onClick={() => setNotice(undefined)}>×</button>
      </div>}
      {at.page === "machines" && <MachineList rows={machines.items} error={machines.error} actions={actions} revision={revision} />}
      {at.page === "machine" && <><Callout error={match.error} />{!match.data && !match.error && <Skeleton className="h-24"/>}{match.data && <MachineView key={match.data.machine_id} machine={match.data} tab={at.tab ?? "overview"} actions={actions} revision={revision} />}</>}
      {catalog && <><Callout error={machines.error}/><Catalog key={catalog} page={catalog} machines={machines.items} actions={actions} revision={revision}/></>}
      {at.page === "volumes" && <VolumeCatalog machines={machines.items} actions={actions} revision={revision} />}
      {at.page !== "machine" && at.page !== "machines" && at.page !== "volumes" && !catalog && <System key={at.page} tab={at.page as SystemTab} id={at.id} actions={actions} revision={revision} operationIds={ops} />}
    {config && <Dialog config={config} close={() => { setDialog(undefined); retry.current = undefined }} />}
  </Shell>
}

createRoot(document.getElementById("root")!).render(<App />)
