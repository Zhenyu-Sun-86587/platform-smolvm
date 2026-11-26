import { useCallback, useEffect, useRef, useState } from "react"
import type { Machine, Service } from "../../../../sdk/typescript/src/index"
import { service } from "../../../../sdk/typescript/src/index"
import { api, Callout } from "../../common"
import { machineHref } from "../../routes"
import { Button } from "../../daytona/button"

function httpsOrigin(value?: string) {
  if (!value) return
  try {
    const url = new URL(value)
    if (url.protocol === "https:" && (value === url.origin || value === url.origin + "/")) return url.origin
  } catch { return undefined }
}

function usePreview(item: Service | undefined, origin: string | undefined, machineAddress: boolean) {
  const [error, setError] = useState<unknown>()
  const [opening, setOpening] = useState(false)
  const request = useRef<AbortController | undefined>(undefined)
  const id = item?.service_id
  const machineID = item?.machine_id
  const enabled = item?.enabled
  useEffect(() => {
    setError(undefined)
    setOpening(false)
    return () => { request.current?.abort(); request.current = undefined }
  }, [origin, id, item?.revision, enabled, machineAddress])
  const open = useCallback(async (target: "_blank" | "_self") => {
    if (!id || !machineID || !origin || request.current || enabled === false) return
    const controller = new AbortController()
    request.current = controller
    setOpening(true)
    setError(undefined)
    try {
      const path = service(id) + "/preview-ticket" + (machineAddress ? "?address=machine" : "")
      const response = await api.response(path, { method: "POST", signal: controller.signal })
      const { action, ticket } = await response.json()
      if (controller.signal.aborted || (target === "_self" && location.hash.split("?")[0] !== machineHref(machineID, "networking"))) return
      if (action !== origin + "/.sandbox/session" || typeof ticket !== "string" || !ticket.trim()) {
        throw new Error("The preview ticket does not match the requested HTTPS address.")
      }
      const form = document.createElement("form")
      form.method = "POST"
      form.action = action
      form.target = target
      form.rel = "noopener"
      form.hidden = true
      const input = document.createElement("input")
      input.name = "ticket"
      input.value = ticket
      form.append(input)
      document.body.append(form)
      try { form.submit() } finally { form.remove() }
    } catch (cause) {
      if (!controller.signal.aborted) setError(cause)
    } finally {
      if (request.current === controller) { request.current = undefined; setOpening(false) }
    }
  }, [id, machineID, origin, enabled, machineAddress])
  return { open, opening, error }
}

export function PreviewButton({ item }: { item: Service }) {
  const origin = httpsOrigin(item.public_host ? "https://" + item.public_host : undefined)
  const preview = usePreview(item, origin, false)
  return <><Button size="sm" variant="outline" disabled={!origin || item.enabled === false || preview.opening}
    onClick={() => void preview.open("_blank")}>{preview.opening ? "Opening…" : "Open preview ↗"}</Button>
    <Callout error={preview.error} /></>
}

export function MachineWebAddress({ machine, services, openWeb, servicesError }: {
  machine: Machine; services?: Service[]; openWeb?: boolean; servicesError?: unknown
}) {
  const origin = httpsOrigin(machine.web_url)
  const [error, setError] = useState<unknown>()
  const [copied, setCopied] = useState("")
  const candidates = services?.filter(item => item.machine_id === machine.machine_id && item.enabled === true &&
    item.public_host && (item.protocol === "http" || item.protocol === "https"))
  const named = candidates?.filter(item => item.name === "web")
  const target = named?.length === 1 ? named[0] : candidates?.length === 1 ? candidates[0] : undefined
  const preview = usePreview(target, origin, true)
  useEffect(() => {
    if (!openWeb || !origin || (!services && !servicesError)) return
    const [path, query] = location.hash.split("?")
    const params = new URLSearchParams(query)
    if (path !== machineHref(machine.machine_id, "networking") || params.get("open") !== "web") return
    params.delete("open")
    history.replaceState(history.state, "", path + (params.size ? "?" + params : ""))
    if (target && !servicesError) void preview.open("_self")
  }, [openWeb, origin, services, servicesError, machine.machine_id, target, preview.open])
  if (!origin) return openWeb ? <p className="subtle">This machine has no published web address.</p> : null
  return <section aria-label="Machine web address">
    <h3>Machine web address</h3><p><code style={{overflowWrap:"anywhere"}}>{origin}</code></p>
    <div className="actions"><Button size="sm" variant="outline" onClick={() =>
      navigator.clipboard.writeText(origin).then(() => { setCopied(origin); setError(undefined) }).catch(setError)
    }>{copied === origin ? "Copied" : "Copy URL"}</Button>
    {target && <Button size="sm" variant="outline" disabled={preview.opening} onClick={() => void preview.open("_blank")}>
      {preview.opening ? "Opening…" : "Open machine preview ↗"}</Button>}</div>
    {!target && <p className="subtle">{servicesError ? "Published services could not be loaded." : !services ? "Checking published HTTP services…"
      : candidates?.length ? "Name one enabled HTTP service web to select it for this address."
      : "Publish an HTTP or HTTPS service to preview this address."}</p>}
    <Callout error={error || preview.error} />
  </section>
}
