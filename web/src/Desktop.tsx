import { useEffect, useRef, useState } from "react"
import type { DesktopSession, Machine, Page, Service } from "../../sdk/typescript/src/index"
import type RFB from "@novnc/novnc"
import { api, Callout, machinePowerState, Section, segment, str, time, useRemote } from "./common"
import { Button } from "./daytona/button"

export default function Desktop({ machine, revision }: { machine: Machine; revision: number }) {
  const base = "machines/" + segment(machine.machine_id)
  const services = useRemote<Page<Service>>(base + "/services", revision)
  const sessions = useRemote<Page<DesktopSession>>(base + "/desktop-sessions", revision)
  const targets = services.data?.items.filter(item => item.protocol === "vnc" && item.enabled !== false) ?? []
  const [selection, setSelection] = useState("")
  const target = targets.find(item => item.service_id === selection) ?? targets[0]
  const [session, setSession] = useState<DesktopSession>()
  const [connection, setConnection] = useState(0)
  const [state, setState] = useState("Disconnected")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [fields, setFields] = useState<string[]>([])
  const surface = useRef<HTMLDivElement>(null)
  const viewer = useRef<RFB | null>(null)

  const reusable = sessions.data?.items.find(item => item.service_id === target?.service_id && item.state === "active" &&
    item.binding_id === machine.binding?.binding_id && item.execution_epoch === machine.binding?.execution_epoch &&
    Date.parse(item.expires_at) > Date.now())

  async function connect() {
    if (!target) return
    setBusy(true)
    setError(undefined)
    try {
      const current = session?.service_id === target.service_id && session.binding_id === machine.binding?.binding_id &&
        session.execution_epoch === machine.binding?.execution_epoch && Date.parse(session.expires_at) > Date.now() ? session : reusable
      const next = current ?? await api.createDesktopSession(machine.machine_id, target.service_id, undefined, {
        revision: machine.revision, generation: machine.binding_generation,
      })
      setSession(next)
      setConnection(value => value + 1)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  async function close() {
    const current = session ?? reusable
    if (!current) return
    setBusy(true)
    try {
      await api.closeDesktopSession(current.desktop_session_id)
      setSession(undefined)
      setConnection(0)
      setState("Session closed")
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  useEffect(() => {
    if (!session || connection === 0 || !surface.current) return
    const target = surface.current
    const cancel = new AbortController()
    let socket: WebSocket | undefined
    let rfb: RFB | undefined
    setFields([])
    setState("Connecting")
    void (async () => {
      try {
        const {default: Viewer} = await import("@novnc/novnc")
        socket = await api.desktopSocket(session.desktop_session_id, cancel.signal)
        if (cancel.signal.aborted) { socket.close(); return }
        rfb = new Viewer(target, socket)
        viewer.current = rfb
        rfb.scaleViewport = true
        rfb.addEventListener("connect", () => { if (!cancel.signal.aborted) setState("Connected") })
        rfb.addEventListener("disconnect", () => { if (!cancel.signal.aborted) setState("Disconnected") })
        rfb.addEventListener("securityfailure", () => {
          if (!cancel.signal.aborted) setError(new Error("The VNC server rejected authentication."))
        })
        rfb.addEventListener("credentialsrequired", event => {
          if (cancel.signal.aborted) return
          const kinds = (event as CustomEvent<{types: string[]}>).detail.types
          setFields(kinds.filter(kind => ["username", "password", "target"].includes(kind)))
        })
      } catch (err) {
        if (!cancel.signal.aborted) { setError(err); setState("Disconnected") }
      }
    })()
    return () => {
      cancel.abort()
      rfb?.disconnect()
      if (socket && socket.readyState < WebSocket.CLOSING) socket.close()
      viewer.current = null
    }
  }, [session, connection])

  return <Section title="Desktop" actions={<>
    <Button onClick={connect} disabled={busy || !target || state === "Connecting" || state === "Connected"}>
      {session || reusable ? "Reconnect" : "Connect"}
    </Button>
    {(session || reusable) && <Button onClick={close} disabled={busy} variant="outline">End session</Button>}
  </>}>
    <Callout error={error || services.error || sessions.error} />
    {services.data && !target && <p className="empty"><a href={"#/" + base + "/networking"}>Publish a VNC service</a> to connect to this desktop.</p>}
    {targets.length > 1 && <label className="field"><span>VNC service</span><select value={target?.service_id} disabled={state === "Connected" || state === "Connecting"}
      onChange={event => setSelection(event.target.value)}>
      {targets.map(item => <option key={item.service_id} value={item.service_id}>{item.name}</option>)}
    </select></label>}
    {target && <div className="row" role="status"><span>{state}</span><span>VM {machinePowerState(machine)}</span>
      {(session || reusable) && <span>Expires {time((session ?? reusable)?.expires_at)}</span>}
    </div>}
    {!!fields.length && <form className="panel-footer" onSubmit={event => {
      event.preventDefault()
      const values = new FormData(event.currentTarget)
      viewer.current?.sendCredentials(Object.fromEntries(fields.map(name => [name, str(values, name)])))
      setFields([])
    }}>
      {fields.map(name => <label key={name} className="field"><span>{name === "target" ? "VNC target" : name}</span>
        <input name={name} type={name === "password" ? "password" : "text"} required />
      </label>)}
      <Button type="submit">Authenticate</Button>
    </form>}
    <div ref={surface} aria-label="Remote desktop" style={{minHeight: session ? '480px' : undefined, height: session ? '60vh' : undefined}} />
  </Section>
}
