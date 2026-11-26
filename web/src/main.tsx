import { ArrowLeft } from "lucide-react"
import { lazy, Suspense, useEffect, useRef, useState } from "react"
import type { FormEvent } from "react"
import { createRoot } from "react-dom/client"
import { PlatformError } from "../../sdk/typescript/src/index"
import type { Control, Machine, Mutation, Operation } from "../../sdk/typescript/src/index"
import { api, fixtureHash, fixtureScene, useCollection } from "./api"
import { Callout, Dialog, isAuthError, label, segment, State, str, useRemote } from "./common"
import type { DialogConfig } from "./common"
import { MachineView } from "./machine"
import { System } from "./system"
import type { SystemTab } from "./system"
import { Brand, Shell, SourceLinks } from "./layout"
import { Dashboard, MachineList } from "./machines"
import { Catalog } from "./catalog"
import { VolumeCatalog } from "./volumes"
import { machineHref, readRoute } from "./routes"
import type { Route } from "./routes"
import { DetailDrawer, EmptyState, Skeleton } from "./ui"
import "./style.css"

const Desktop = lazy(() => import("./Desktop"))
const Terminal = lazy(() => import("./Terminal"))
const operationStorage = fixtureScene ? "sandbox.fixture.operations" : "sandbox.operations"
if (fixtureHash && !location.hash) history.replaceState(history.state, "", fixtureHash)
const fixtureOnly = new Set(["agents", "domains", "integrations", "environment"])
const machinesRoute = { page: "machines" } as const

type WriteOptions = { revision?: number; method?: string; intent?: Mutation }
type TrackedOptions = { key?: string; intent?: Mutation }
export type Actions = {
  dialog: (config: DialogConfig) => void
  mutate: <T = Operation>(path: string, body?: unknown, options?: WriteOptions) => Promise<T>
  tracked: <T>(action: (intent: Mutation) => Promise<T>, options?: TrackedOptions) => Promise<T>
  refresh: () => void
}

type PlatformControl = Control & { runtime_ready?: boolean; principal?: { username?: string } }
type GlobalRoute = Extract<Route, { action?: "new" }>
type View = { at: Route; background: GlobalRoute }
const isGlobal = (route: Route): route is GlobalRoute =>
  route.page !== "machine" && route.page !== "terminal" && route.page !== "desktop"
const initialView = (): View => {
  const at = readRoute(location.hash || fixtureHash || "")
  return { at, background: isGlobal(at) ? at : machinesRoute }
}

function restoreOperations(): string[] {
  try {
    const value: unknown = JSON.parse(sessionStorage.getItem(operationStorage) ?? "[]")
    if (Array.isArray(value)) return value.filter((id): id is string => typeof id === "string")
  } catch {}
  return []
}

function Login({ ready }: { ready: () => void }) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    setBusy(true)
    setError(undefined)
    try {
      await api.login(str(form, "username"), str(form, "password"))
      ready()
    } catch (error) {
      setError(error)
    } finally {
      setBusy(false)
    }
  }
  return <main className="login"><section><Brand /><h1>Sign in</h1><form onSubmit={submit}>
    <label className="field"><span>Username</span><input name="username" autoComplete="username" autoFocus required /></label>
    <label className="field"><span>Password</span><input name="password" type="password" autoComplete="current-password" required /></label>
    <Callout error={error} /><button className="primary" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
  </form><SourceLinks /></section></main>
}

const mayHaveEffect = (error: unknown) => !(error instanceof PlatformError) ||
  error.detail.effect === "unknown" || error.detail.effect === "partial" || error.detail.effect === "applied" ||
  Boolean(error.detail.operation_id) || error.detail.retry === "verify_original_intent" || error.status >= 500 || error.status === 408

function App() {
  const [{ at, background }, setView] = useState(initialView)
  const [auth, setAuth] = useState<"checking" | "signed_in" | "signed_out">("checking")
  const [sessionError, setSessionError] = useState<unknown>()
  const [revision, setRevision] = useState(0)
  const [control, setControl] = useState<PlatformControl>()
  const [config, setDialog] = useState<DialogConfig>()
  const [notice, setNotice] = useState<Operation>()
  const [ops, setOps] = useState<string[]>(restoreOperations)
  const [streamError, setStreamError] = useState<unknown>()
  const streamID = useRef("")
  const intents = useRef(new Map<string, Promise<Mutation>>())
  const watching = useRef(new Map<string, AbortController>())
  const latestOperation = useRef("")
  const session = useRef<AbortController | undefined>(undefined)
  const refresh = () => setRevision(value => value + 1)

  useEffect(() => {
    const update = () => {
      const next = readRoute(location.hash || fixtureHash || "")
      setView(previous => ({
        at: next,
        background: isGlobal(next) ? next : previous.background,
      }))
    }
    window.addEventListener("hashchange", update)
    return () => window.removeEventListener("hashchange", update)
  }, [])
  useEffect(() => { window.scrollTo(0, 0) }, [background.page])

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
    const active = watching.current
    const pending = intents.current
    const abort = new AbortController()
    session.current = abort
    return () => {
      abort.abort()
      for (const observe of active.values()) observe.abort()
      active.clear()
      pending.clear()
      latestOperation.current = ""
    }
  }, [auth])

  const machines = useCollection<Machine>(auth === "signed_in" ? "machines" : "", revision)
  const selectedId = isGlobal(at) ? undefined : at.id
  const match = useRemote<Machine>(auth === "signed_in" && selectedId ? "machines/" + segment(selectedId) : "", revision)
  useEffect(() => {
    if (isAuthError(machines.error) || isAuthError(match.error)) setAuth("signed_out")
  }, [machines.error, match.error])

  const track = <T,>(result: T) => {
    if (result && typeof result === "object" && "operation_id" in result && typeof result.operation_id === "string") {
      const id = result.operation_id
      latestOperation.current = id
      setOps(previous => {
        const next = [id, ...previous.filter(item => item !== id)].slice(0, 40)
        try { sessionStorage.setItem(operationStorage, JSON.stringify(next)) } catch {}
        return next
      })
      if ("state" in result) setNotice(result as unknown as Operation)
      if (!watching.current.has(id)) {
        const abort = new AbortController()
        watching.current.set(id, abort)
        void api.waitOperation(id, {
          signal: abort.signal,
          onUpdate: operation => {
            if (!abort.signal.aborted && latestOperation.current === id) setNotice(operation)
          },
        }).then(refresh).catch(error => {
          if (abort.signal.aborted) return
          if (isAuthError(error)) setAuth("signed_out")
          else setStreamError(error)
        }).finally(() => {
          if (watching.current.get(id) === abort) watching.current.delete(id)
        })
      }
    }
    refresh()
    return result
  }

  const send = async <T,>(run: (intent: Mutation) => Promise<T>, key?: string, explicit?: Mutation) => {
    const startedIn = session.current
    const pending = intents.current
    const original = key === undefined ? undefined : pending.get(key)
    const intent = explicit ? Promise.resolve(explicit) : original ?? api.intent()
    if (key !== undefined && !explicit && !original) pending.set(key, intent)
    const forget = () => {
      if (key !== undefined && pending.get(key) === intent) pending.delete(key)
    }
    let mutation: Mutation
    try {
      mutation = await intent
    } catch (error) {
      forget()
      if (isAuthError(error)) setAuth("signed_out")
      throw error
    }
    try {
      startedIn?.signal.throwIfAborted()
      const result = await run(mutation)
      forget()
      return startedIn?.signal.aborted ? result : track(result)
    } catch (error) {
      if (!mayHaveEffect(error)) forget()
      if (!startedIn?.signal.aborted && isAuthError(error)) setAuth("signed_out")
      throw error
    }
  }
  const actions: Actions = {
    dialog: setDialog,
    mutate: <T,>(path: string, body: unknown = {}, options?: WriteOptions) => send(
      intent => api.mutate<T>(path, body, { ...options, intent }),
      JSON.stringify([options?.method ?? "POST", path, options?.revision, body]), options?.intent,
    ),
    tracked: <T,>(action: (intent: Mutation) => Promise<T>, options?: TrackedOptions) =>
      send(action, options?.key === undefined ? undefined : "tracked:" + options.key, options?.intent),
    refresh,
  }

  if (auth === "checking") return <main className="login"><section><Brand /><p>Connecting to platform</p><Callout error={sessionError} />
    {sessionError ? <button onClick={() => location.reload()}>Try again</button> : null}
  </section></main>
  if (auth === "signed_out") return <Login ready={() => { streamID.current = ""; setAuth("checking") }} />

  const signOut = () => {
    api.logout().then(() => {
      streamID.current = ""
      try { sessionStorage.removeItem(operationStorage) } catch {}
      setOps([])
      setNotice(undefined)
      setDialog(undefined)
      setStreamError(undefined)
      setAuth("signed_out")
    }).catch(setStreamError)
  }
  const dialog = config && <Dialog config={config} close={() => setDialog(undefined)} />
  if (at.page === "terminal" || at.page === "desktop") {
    const title = at.page === "desktop" ? "Desktop" : "Terminal"
    return <main className="standalone-console" data-app-shortcuts="off">
      {!match.data && <header className="console-page-header">
        <a className="console-return" href={machineHref(at.id)}><ArrowLeft size={16} /> Back to machine</a>
        <strong>{title}</strong>
      </header>}
      <Callout error={match.error} />
      {!match.data && !match.error && <Skeleton className="h-24" />}
      {match.data && <Suspense fallback={<p className="empty">Loading {title.toLowerCase()}…</p>}>
        {at.page === "desktop" ? <Desktop key={at.id} machine={match.data} revision={revision} />
          : <Terminal key={at.execId ?? at.id} execId={at.execId} machine={match.data} />}
      </Suspense>}
      {dialog}
    </main>
  }

  const page = background.page
  const catalog = page === "snapshots" || page === "tasks" || page === "networking" ? page : undefined
  const machineProps = { rows: machines.items, error: machines.error, actions, revision, defaultBoot: control?.default_boot_mode,
    create: background.action === "new" && at.page !== "machine" }
  return <Shell page={page} machines={machines.items} runtime={control?.runtime_version} ready={control?.runtime_ready}
    username={control?.principal?.username} signOut={signOut} revision={revision}>
    {({ onSearch }) => <>
      {control?.maintenance && <div className="callout" role="status"><strong>Maintenance mode</strong></div>}
      <Callout error={streamError} />
      {notice && <div className={`operation-notice ${notice.error ? "operation-error" : ""}`} role="status">
        <State value={notice.state} />
        <a href={`#/operations/${segment(notice.operation_id)}`}>{label(notice.kind ?? "Operation")} · {notice.stage ?? notice.operation_id}</a>
        {notice.effect === "partial" && <State value="partial" />}
        {notice.effect === "unknown" && <span>Effect unknown</span>}
        {notice.error?.message && <span>{notice.error.message}</span>}
        {notice.result?.status === "requires_restart" && <span>Resource changes require restart</span>}
        <button aria-label="Dismiss operation" onClick={() => { latestOperation.current = ""; setNotice(undefined) }}>×</button>
      </div>}
      {page === "dashboard" && <Dashboard {...machineProps} onSearch={onSearch} username={control?.principal?.username} />}
      {page === "machines" && <MachineList {...machineProps} />}
      {catalog && <><Callout error={machines.error} /><Catalog key={catalog} page={catalog} machines={machines.items} actions={actions} revision={revision} /></>}
      {page === "volumes" && <VolumeCatalog machines={machines.items} actions={actions} revision={revision} />}
      {page !== "dashboard" && page !== "machines" && page !== "volumes" && !catalog &&
        (fixtureOnly.has(page) && !fixtureScene ? <EmptyState title="Feature unavailable" description="This workspace does not provide this capability." />
          : <System key={page} tab={page as SystemTab} id={background.id} actions={actions} revision={revision}
            operationIds={ops} {...{ create: background.action === "new" }} />)}
      <DetailDrawer open={at.page === "machine"} onOpenChange={open => { if (!open) location.hash = "#/" + page }}
        title={match.data?.name ?? "Machine details"}>
        <Callout error={match.error} />
        {!match.data && !match.error && <Skeleton className="h-24" />}
        {at.page === "machine" && match.data &&
          <MachineView key={match.data.machine_id} machine={match.data} tab={at.tab} openWeb={at.openWeb} actions={actions} revision={revision} />}
      </DetailDrawer>
      {dialog}
    </>}
  </Shell>
}

createRoot(document.getElementById("root")!).render(<App />)
