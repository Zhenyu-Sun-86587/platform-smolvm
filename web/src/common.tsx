import { ReactNode, useEffect, useState } from "react"
import { PlatformError } from "../../sdk/typescript/src/index"
import type { Machine } from "../../sdk/typescript/src/index"

import { api } from "./api"
import { Dialog as Root, DialogContent, DialogTitle, DialogDescription, DialogHeader, DialogFooter } from "./daytona/dialog"
import { Button } from "./daytona/button"
import { AlertCircle } from "lucide-react"
export { api, segment } from "./api"
export const query = (value: string) => new URLSearchParams({ path: value }).toString()
export const label = (value: string) => value.replace(/_/g, " ").replace(/^./, char => char.toUpperCase())
export const isAuthError = (error: unknown) => error instanceof PlatformError && error.status === 401

export const bundleBound = (m: Machine) => Boolean(m.binding?.runtime_bundle_id || m.binding?.runtime_instance_id)
export function machinePowerState(m: Machine) {
  if (m.power_state) return m.power_state
  if (bundleBound(m)) return "unknown"
  if (m.observed_state === "paused") return "hibernated"
  if (m.observed_state === "created") return "stopped"
  return m.observed_state
}

export function fullStateBlock(m: Machine, key: string) {
  const capability = m.capabilities?.[key]
  if (capability?.status === "supported" && capability.supported) return undefined
  return capability?.message ?? capability?.reason_code ?? "Capability has not been verified for this machine."
}

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
      setData(undefined)
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
    <AlertCircle size={16} /><strong>{detail?.code ?? "Request failed"}</strong><span>{message}</span>
    {detail?.operation_id && <span>Operation {detail.operation_id}</span>}
    {detail?.effect && <span>Effect {detail.effect}</span>}
    {detail?.retry && <span>Retry {label(detail.retry)}</span>}
  </div>
}

export function State({ value }: { value?: string | null }) {
  if (!value) return <span className="muted">—</span>
  const tone = ["running", "ready", "succeeded", "active", "enabled"].includes(value) ? "good"
    : ["failed", "recovery_required", "unknown", "degraded", "interrupted"].includes(value) ? "bad"
      : ["paused", "queued", "observing", "dispatching", "starting", "suspect", "not_ready", "cancelling", "verification_pending"].includes(value) ? "pending" : "neutral"
  return <span className={`state ${tone}`}><i />{value==="paused"?"Hibernated":label(value)}</span>
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
  options?: { value: string; label?: string; disabled?: boolean }[]
  value?: string | boolean | number
  required?: boolean
  min?: number
  note?: string
}
export type DialogConfig = {
  title: string
  submit?: string
  description?: string
  content?: ReactNode
  sheet?: boolean
  destructive?: boolean
  fields?: Field[]
  act: (data: FormData) => Promise<unknown>
}

export function Dialog({ config, close }: { config: DialogConfig; close: () => void }) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  return <Root open onOpenChange={open => { if (!open && !busy) close() }}>
    <DialogContent placement={config.sheet?"right":"center"} animate={!config.sheet} className={config.sheet ? "form-sheet" : "form-dialog"} onEscapeKeyDown={event => { if (busy) event.preventDefault() }} onInteractOutside={event => { if (busy) event.preventDefault() }} showCloseButton={!busy} {...(config.description ? {} : {"aria-describedby":undefined})}>
      <DialogHeader><DialogTitle>{config.title}</DialogTitle>
      {config.description && <DialogDescription>{config.description}</DialogDescription>}</DialogHeader>
      {config.content}
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
            {field.options.map(option => <option key={option.value} value={option.value} disabled={option.disabled}>{option.label ?? option.value}</option>)}
          </select>
            : field.type === "textarea" ? <textarea name={field.name} defaultValue={String(field.value ?? "")} required={field.required} rows={4} />
              : field.type === "checkbox" ? <input type="checkbox" name={field.name} defaultChecked={Boolean(field.value)} />
                : <input type={field.type ?? "text"} name={field.name} defaultValue={field.type === "file" ? undefined : String(field.value ?? "")} min={field.min} required={field.required} />}
          {field.note && <small>{field.note}</small>}
        </label>)}
        <Callout error={error} />
        <DialogFooter><Button variant="outline" type="button" onClick={close} disabled={busy}>Cancel</Button><Button variant={config.destructive ? "destructive" : "default"} type="submit" disabled={busy}>{busy ? "Submitting…" : config.submit ?? config.title}</Button></DialogFooter>
      </form>
    </DialogContent>
  </Root>
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
