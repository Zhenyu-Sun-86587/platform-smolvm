import { ReactNode, useEffect, useState } from "react"
import { Client, PlatformError } from "../../sdk/typescript/src/index"

export const api = new Client(location.origin)
export const segment = encodeURIComponent
export const query = (value: string) => new URLSearchParams({ path: value }).toString()
export const label = (value: string) => value.replace(/_/g, " ").replace(/^./, char => char.toUpperCase())
export const isAuthError = (error: unknown) => error instanceof PlatformError && error.status === 401

export function useRemote<T>(path: string, revision: number) {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<unknown>()
  const [loadedPath, setLoadedPath] = useState<string>()
  useEffect(() => {
    if (!path) return
    const abort = new AbortController()
    api.get<T>(path, abort.signal).then(data => {
      if (abort.signal.aborted) return
      setData(data)
      setError(undefined)
      setLoadedPath(path)
    }).catch(error => {
      if (abort.signal.aborted) return
      setError(error)
      setLoadedPath(path)
    })
    return () => abort.abort()
  }, [path, revision])
  return { data: loadedPath === path ? data : undefined, error: loadedPath === path ? error : undefined }
}

export function Callout({ error }: { error: unknown }) {
  if (!error) return null
  const message = error instanceof Error ? error.message : String(error)
  const detail = error instanceof PlatformError ? error.detail : undefined
  return <div className="callout" role="alert">
    <strong>{detail?.code ?? "Request failed"}</strong><span>{message}</span>
    {detail?.operation_id && <span>Operation {detail.operation_id}</span>}
    {detail?.effect && <span>Effect {detail.effect}</span>}
    {detail?.retry && <span>Retry {label(detail.retry)}</span>}
  </div>
}

export function State({ value }: { value?: string | null }) {
  if (!value) return <span className="muted">—</span>
  const tone = ["running", "ready", "succeeded", "active", "enabled"].includes(value) ? "good"
    : ["failed", "recovery_required", "unknown", "degraded", "interrupted"].includes(value) ? "bad"
      : ["paused", "queued", "observing", "dispatching", "starting", "suspect", "not_ready", "cancelling"].includes(value) ? "pending" : "neutral"
  return <span className={`state ${tone}`}><i />{label(value)}</span>
}

export function Section({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return <section className="panel"><header className="panel-head"><h2>{title}</h2>{actions && <div className="actions">{actions}</div>}</header>{children}</section>
}

export function Details({ value }: { value: Record<string, unknown> }) {
  return <dl className="details">{Object.entries(value).map(([key, data]) => <div key={key}>
    <dt>{label(key)}</dt><dd>{data == null || data === "" ? "—" : typeof data === "object" ? <pre>{JSON.stringify(data, null, 2)}</pre> : String(data)}</dd>
  </div>)}</dl>
}

export function Records({ value }: { value?: unknown }) {
  if (value == null) return <p className="empty">No data returned.</p>
  return <pre className="record">{JSON.stringify(value, null, 2)}</pre>
}

export function download(response: Response, filename: string) {
  return response.blob().then(blob => {
    const href = URL.createObjectURL(blob)
    const link = document.createElement("a")
    link.href = href
    link.download = filename
    link.click()
    setTimeout(() => URL.revokeObjectURL(href), 30000)
  })
}

export function time(value?: string | null) {
  if (!value) return "—"
  const parsed = new Date(value)
  return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleString()
}

type Field = {
  name: string
  label: string
  type?: "text" | "number" | "password" | "checkbox" | "textarea" | "file"
  options?: { value: string; label?: string }[]
  value?: string | boolean | number
  required?: boolean
  min?: number
  note?: string
}
export type DialogConfig = {
  title: string
  submit?: string
  description?: string
  destructive?: boolean
  fields?: Field[]
  act: (data: FormData) => Promise<unknown>
}

export function Dialog({ config, close }: { config: DialogConfig; close: () => void }) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  return <div className="overlay" onClick={event => { if (event.currentTarget === event.target && !busy) close() }}>
    <section role="dialog" aria-modal="true" aria-label={config.title} className="dialog">
      <header><h2>{config.title}</h2><button type="button" aria-label="Close" className="icon-button" onClick={close} disabled={busy}>×</button></header>
      {config.description && <p className="muted">{config.description}</p>}
      <form onSubmit={event => {
        event.preventDefault()
        const form = new FormData(event.currentTarget)
        setBusy(true)
        setError(undefined)
        config.act(form).then(close).catch(setError).finally(() => setBusy(false))
      }}>
        {config.fields?.map(field => <label key={field.name} className={field.type === "checkbox" ? "checkbox" : "field"}>
          <span>{field.label}</span>
          {field.options ? <select name={field.name} defaultValue={String(field.value ?? "")} required={field.required}>
            {field.options.map(option => <option key={option.value} value={option.value}>{option.label ?? option.value}</option>)}
          </select>
            : field.type === "textarea" ? <textarea name={field.name} defaultValue={String(field.value ?? "")} required={field.required} rows={4} />
              : field.type === "checkbox" ? <input type="checkbox" name={field.name} defaultChecked={Boolean(field.value)} />
                : <input type={field.type ?? "text"} name={field.name} defaultValue={field.type === "file" ? undefined : String(field.value ?? "")} min={field.min} required={field.required} />}
          {field.note && <small>{field.note}</small>}
        </label>)}
        <Callout error={error} />
        <footer><button type="button" onClick={close} disabled={busy}>Cancel</button><button className={config.destructive ? "danger" : "primary"} type="submit" disabled={busy}>{busy ? "Submitting…" : config.submit ?? config.title}</button></footer>
      </form>
    </section>
  </div>
}

export const str = (data: FormData, field: string) => String(data.get(field) ?? "")
export const num = (data: FormData, field: string) => Number(str(data, field))
export const argv = (data: FormData) => {
  const command: unknown = JSON.parse(str(data, "argv"))
  if (!Array.isArray(command) || !command.length || command.some(arg => typeof arg !== "string")) {
    throw new Error("Enter a JSON array of command arguments.")
  }
  return command as string[]
}
