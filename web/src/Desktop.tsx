import { useEffect, useRef, useState } from "react"
import { ArrowLeft, Grid2X2, Monitor } from "lucide-react"
import type { DesktopSession, Machine, Mutation, Service } from "../../sdk/typescript/src/index"
import type RFB from "@novnc/novnc"
import { Callout, machinePowerState, str, time } from "./common"
import { api, fixtureScene, segment, useCollection } from "./api"
import { machineHref } from "./routes"
import { Button } from "./ui"
import { DesktopLauncher } from "./features/console/DesktopLauncher"
import styles from "./features/console/console.module.css"

export default function Desktop(props: { machine: Machine; revision: number; embedded?: boolean }) {
  return fixtureScene ? <DesktopFixture machine={props.machine} scene={fixtureScene} /> : <RemoteDesktop {...props} />
}

function RemoteDesktop({ machine, revision, embedded = false }: { machine: Machine; revision: number; embedded?: boolean }) {
  const base = "machines/" + segment(machine.machine_id)
  const services = useCollection<Service>(base + "/services", revision)
  const sessions = useCollection<DesktopSession>(base + "/desktop-sessions", revision)
  const targets = services.items?.filter(item => item.protocol === "vnc" && item.enabled !== false && !item.public_host && !item.public_port) ?? []
  const [selection, setSelection] = useState("")
  const target = targets.find(item => item.service_id === selection) ?? targets[0]
  const [session, setSession] = useState<DesktopSession>()
  const [connection, setConnection] = useState(0)
  const [state, setState] = useState("Disconnected")
  const [busy, setBusy] = useState(false)
  const held = useRef(false)
  const [closedSession, setClosedSession] = useState("")
  const createIntent = useRef<{ key: string; intent: Mutation } | undefined>(undefined)
  const [revoked, setRevoked] = useState(false)
  const [error, setError] = useState<unknown>()
  const [fields, setFields] = useState<string[]>([])
  const surface = useRef<HTMLDivElement>(null)
  const viewer = useRef<RFB | null>(null)

  const reusable = sessions.items?.find(item => item.service_id === target?.service_id && item.state === "active" &&
    item.desktop_session_id !== closedSession && item.binding_id === machine.binding?.binding_id &&
    item.binding_generation === machine.binding_generation && item.execution_epoch === machine.binding?.execution_epoch &&
    Date.parse(item.expires_at) > Date.now())
  const sessionMatches = session && session.service_id === target?.service_id &&
    session.binding_id === machine.binding?.binding_id && session.binding_generation === machine.binding_generation &&
    session.state === "active" && !revoked && Date.parse(session.expires_at) > Date.now()

  async function connect() {
    if (!target || held.current) return
    held.current = true
    setBusy(true)
    setError(undefined)
    try {
      let next = sessionMatches && session.execution_epoch === machine.binding?.execution_epoch ? session : reusable
      if (!next) {
        const key = JSON.stringify([machine.machine_id, machine.binding_generation, machine.revision, target.service_id, target.revision])
        const intent = createIntent.current?.key === key ? createIntent.current.intent : await api.intent()
        createIntent.current = { key, intent }
        next = await api.createDesktopSession(machine.machine_id, target.service_id, undefined, {
          revision: machine.revision, generation: machine.binding_generation, intent,
        })
        createIntent.current = undefined
      }
      setSession(next)
      setRevoked(false)
      setConnection(value => value + 1)
    } catch (err) {
      setError(err)
    } finally {
      held.current = false
      setBusy(false)
    }
  }

  async function close() {
    const current = session ?? reusable
    if (!current || held.current) return
    held.current = true
    setBusy(true)
    try {
      await api.closeDesktopSession(current.desktop_session_id)
      setClosedSession(current.desktop_session_id)
      setSession(undefined)
      setConnection(0)
      setFields([])
      setError(undefined)
      setState("Session closed")
    } catch (err) {
      setError(err)
    } finally {
      held.current = false
      setBusy(false)
    }
  }

  useEffect(() => {
    if (!session || revoked) return
    const expires = Date.parse(session.expires_at)
    const expire = () => {
      setConnection(0)
      setFields([])
      setState("Session expired")
      setRevoked(true)
    }
    const timer = window.setTimeout(expire, Math.max(0, expires - Date.now()))
    return () => clearTimeout(timer)
  }, [session, revoked])

  useEffect(() => {
    if (!session || !connection || revoked) return
    const listed = sessions.items?.find(item => item.desktop_session_id === session.desktop_session_id)
    const laterEpoch = (machine.binding?.execution_epoch ?? 0) > session.execution_epoch
    if (!sessionMatches || laterEpoch || listed && listed.state !== "active") {
      setConnection(0)
      setFields([])
      setState("Session no longer current")
      setRevoked(true)
    }
  }, [session, connection, revoked, sessionMatches, machine.binding?.execution_epoch, sessions.items])

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
        rfb.addEventListener("disconnect", event => {
          if (cancel.signal.aborted) return
          setState("Disconnected")
          if (!(event as CustomEvent<{ clean?: boolean }>).detail?.clean) {
            setError((previous: unknown) => previous ?? new Error("The desktop connection closed before it could continue."))
          }
        })
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

  const connecting = state === "Connecting"
  const connected = state === "Connected"
  return <div className={embedded ? styles.desktopEmbed : styles.console}>
    <div className={styles.toolbar} data-visible={!connected && !connecting}>
      <DesktopHeading machine={machine} embedded={embedded}/>
      <div className={styles.toolbarActions}>
        <span role="status">{state}</span>
        {(session || reusable) && <Button onClick={() => void close()} disabled={busy} size="sm">End session</Button>}
      </div>
    </div>
    <div className={styles.surface}>
      <div ref={surface} className={styles.desktopView} aria-label="Remote desktop" />
      {connecting && fields.length === 0 && error == null && <div className={styles.overlay}>
        <p className={styles.desktopStatus}>Starting desktop… (a sleeping machine may take a few seconds to wake)</p>
      </div>}
      {(!connecting && !connected || fields.length > 0 || error != null) && <div className={styles.overlay}><div className={styles.statusCard}>
        <Callout error={error || services.error || sessions.error} />
        <h2>{connecting ? "Connecting to desktop" : connected ? "Desktop connection" : "Connect to desktop"}</h2>
        <p>{services.items ? target ? `Machine ${machinePowerState(machine)}` : "No private VNC service is configured."
          : services.error ? "Could not load the machine services." : "Checking published services…"}</p>
        {services.items && !target && <a href={machineHref(machine.machine_id, "networking")}>View networking</a>}
        {targets.length > 1 && <label className="field"><span>VNC service</span>
          <select value={target?.service_id} disabled={busy || connecting || connected} onChange={event => setSelection(event.target.value)}>
            {targets.map(item => <option key={item.service_id} value={item.service_id}>{item.name}</option>)}
          </select>
        </label>}
        {(session || reusable) && !revoked && <p>Expires {time((session ?? reusable)?.expires_at)}</p>}
        {target && !connecting && !connected && <Button variant="primary" onClick={() => void connect()} disabled={busy || !sessions.items}>
          {busy ? "Opening…" : sessionMatches || reusable ? "Reconnect" : "Connect"}
        </Button>}
        {fields.length > 0 && <form onSubmit={event => {
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
      </div></div>}
    </div>
  </div>
}

function DesktopHeading({ machine, embedded }: { machine: Machine; embedded?: boolean }) {
  return <div className={styles.toolbarName}>
    {!embedded && <a href={machineHref(machine.machine_id)} aria-label="Back to machine"><ArrowLeft size={16}/></a>}
    <Monitor size={16}/><strong>{machine.name}</strong><span>Desktop</span>
  </div>
}

function DesktopFixture({ machine, scene }: { machine: Machine; scene: string }) {
  const loading = scene === "32"
  const disconnected = scene === "desktop-disconnected"
  const [open, setOpen] = useState(["34", "39", "40", "launcher-failed"].includes(scene))
  useEffect(() => {
    if (open || disconnected) return
    function shortcut(event: KeyboardEvent) {
      if (!(event.ctrlKey || event.metaKey) || event.key.toLowerCase() !== "b") return
      event.preventDefault()
      setOpen(true)
    }
    window.addEventListener("keydown", shortcut)
    return () => window.removeEventListener("keydown", shortcut)
  }, [open, disconnected])

  return <div className={styles.console}>
    <div className={styles.toolbar} data-visible={disconnected}>
      <DesktopHeading machine={machine} />
      <div className={styles.toolbarActions}><span role="status">{loading ? "Connecting" : disconnected ? "Disconnected" : "Connected"}</span></div>
    </div>
    <div className={styles.launchStrip}>
      <button type="button" onClick={() => setOpen(true)} disabled={disconnected}><Grid2X2 size={14} /> Launch <kbd>⌘B</kbd></button>
    </div>
    <div className={`${styles.surface} ${styles.officeFixture}`} data-loading={loading}>
      {(loading || disconnected) && <div className={styles.overlay}><p className={styles.desktopStatus}>
        {loading ? "Starting desktop… (a sleeping machine may take a few seconds to wake)" : "The guest display connection has ended."}
      </p></div>}
    </div>
    <DesktopLauncher open={open} onOpenChange={setOpen} scene={scene} />
  </div>
}
