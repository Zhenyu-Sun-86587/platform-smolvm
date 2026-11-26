import { useEffect, useRef, useState } from "react"
import { ArrowLeft, SquareTerminal } from "lucide-react"
import type { Machine } from "../../sdk/typescript/src/index"
import { Terminal as Xterm } from "@xterm/xterm"
import { FitAddon } from "@xterm/addon-fit"
import "@xterm/xterm/css/xterm.css"
import { api, fixtureScene, segment } from "./api"
import { Callout } from "./common"
import { machineHref } from "./routes"
import { Button, mobileQuery, useMobile } from "./ui"
import styles from "./features/console/console.module.css"

const fixtureText = "$ whoami\r\nroot\r\n$ pwd\r\n/workspace\r\n$ "

export default function Terminal({ execId, machine, embedded = false }: { execId?: string; machine?: Machine; embedded?: boolean }) {
  const page = useRef<HTMLDivElement>(null)
  const host = useRef<HTMLDivElement>(null)
  const retry = useRef<() => void>(() => {})
  const [message, setMessage] = useState("Connecting")
  const [error, setError] = useState<unknown>()
  const mobile = useMobile()
  const machineID = machine?.machine_id

  useEffect(() => {
    const viewport = window.visualViewport
    const node = page.current
    if (!viewport || embedded || !node) return
    const resize = () => node.style.setProperty("--console-viewport", viewport.height + "px")
    resize()
    viewport.addEventListener("resize", resize)
    return () => viewport.removeEventListener("resize", resize)
  }, [embedded])

  useEffect(() => {
    if (!host.current || !execId) return
    const root = host.current
    const term = new Xterm({
      fontSize: 13, fontFamily: "'JetBrains Mono', ui-monospace, monospace",
      cursorBlink: true, convertEol: false, scrollback: 3000,
      theme: { background: "#0d1422", foreground: "#e6edf9", cursor: "#96c0ff", selectionBackground: "#30466a" },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(root)
    const viewport = matchMedia(mobileQuery)
    const resizeScreen = () => { term.options.fontSize = viewport.matches ? 12 : 13; fit.fit() }
    const observer = new ResizeObserver(resizeScreen)
    observer.observe(root)
    resizeScreen()

    if (fixtureScene) {
      const fixtureState = fixtureScene === "terminal-connecting" ? "Connecting"
        : fixtureScene === "terminal-exited" ? "Session exited (0)"
        : fixtureScene === "terminal-reconnect" ? "Disconnected" : "Connected"
      if (fixtureState !== "Connecting") term.write(fixtureText)
      if (fixtureScene === "terminal-gap") term.writeln("\r\n[Earlier terminal output is unavailable]")
      setMessage(fixtureState)
      retry.current = () => setMessage("Connected")
      return () => { observer.disconnect(); term.dispose() }
    }

    const abort = new AbortController()
    const path = "execs/" + segment(execId)
    let socket: WebSocket | undefined
    let cursor = 0
    let seq = 0
    let ready = false
    let exited = false
    let connecting = false
    const send = (data: object) => {
      if (ready && socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(data))
    }
    const resize = term.onResize(({ cols, rows }) => send({ type: "resize", cols, rows }))
    const input = term.onData(data => {
      if (!ready || socket?.readyState !== WebSocket.OPEN) return
      const bytes = new TextEncoder().encode(data)
      for (let offset = 0; offset < bytes.length; offset += 4096) {
        const part = bytes.subarray(offset, offset + 4096)
        send({ type: "stdin", seq: ++seq, data_base64: btoa(String.fromCharCode(...part)) })
      }
    })
    const connect = async () => {
      if (connecting || exited || socket && socket.readyState < WebSocket.CLOSING) return
      connecting = true
      setError(undefined)
      setMessage("Connecting")
      try {
        if (machineID) {
          const session = await api.get<{ machine_id: string }>(path, abort.signal)
          if (session.machine_id !== machineID) throw new Error("This terminal session belongs to another machine.")
        }
        const ws = await api.browserSocket(
          path + "/attach?cursor=" + cursor, path + "/attach-ticket", "sandbox.terminal.v1", abort.signal
        )
        if (abort.signal.aborted) { ws.close(); return }
        socket = ws
        ws.binaryType = "arraybuffer"
        ws.onmessage = event => {
          if (event.data instanceof ArrayBuffer) {
            cursor += event.data.byteLength
            term.write(new Uint8Array(event.data))
            return
          }
          try {
            const frame = JSON.parse(event.data)
            if (frame.type === "exit") {
              exited = true
              ready = false
              setMessage(frame.exit_code == null ? "Session exited" : `Session exited (${frame.exit_code})`)
            } else if (frame.type === "gap") {
              if (Number.isSafeInteger(frame.to) && frame.to > cursor) cursor = frame.to
              term.writeln("\r\n[Earlier terminal output is unavailable]")
            } else if (frame.type === "error") {
              setError(new Error(frame.message ?? frame.error?.message ?? "Terminal stream failed"))
            } else if (frame.type === "ready" && Number.isSafeInteger(frame.stdin_seq)) {
              seq = frame.stdin_seq
              ready = true
              setMessage("Connected")
              send({ type: "resize", cols: term.cols, rows: term.rows })
              term.focus()
            }
          } catch { setError(new Error("Invalid terminal control frame")) }
        }
        ws.onerror = () => { if (!abort.signal.aborted) setError(new Error("Terminal connection failed")) }
        ws.onclose = () => {
          ready = false
          if (!abort.signal.aborted && !exited) setMessage("Disconnected")
        }
      } catch (error) {
        if (!abort.signal.aborted) { setError(error); setMessage("Disconnected") }
      } finally {
        connecting = false
      }
    }
    retry.current = () => void connect()
    void connect()
    return () => {
      abort.abort()
      socket?.close()
      observer.disconnect()
      input.dispose()
      resize.dispose()
      term.dispose()
    }
  }, [execId, machineID])

  return <div className={embedded ? styles.terminalEmbed : styles.console} ref={page}>
    <div className={styles.toolbar}>
      <div className={styles.toolbarName}>
        {!embedded && <a href={machineID ? machineHref(machineID, "terminal") : "#/machines"} aria-label="Back to terminal sessions"><ArrowLeft size={16}/></a>}
        <SquareTerminal size={16}/><strong>{machine?.name ?? "Terminal"}</strong>
        {!mobile && execId && <code>{execId}</code>}
      </div>
      <div className={styles.toolbarActions}><span role="status">{execId ? message : "No session selected"}</span>
        {execId && message === "Disconnected" && <Button size="sm" onClick={() => retry.current()}>Reconnect</Button>}
      </div>
    </div>
    <Callout error={error} />
    {execId ? <div className={styles.screen} ref={host} /> :
      <div className={styles.surface}><div className={styles.overlay}><section className={styles.statusCard}>
        <h2>Select a terminal session</h2>
        <p>Open a shell or attach to an existing execution from the machine workspace.</p>
        <a href={machineID ? machineHref(machineID, "terminal") : "#/machines"}>Browse sessions</a>
      </section></div></div>}
  </div>
}
