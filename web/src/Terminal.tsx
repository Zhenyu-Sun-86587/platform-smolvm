import { useEffect, useRef, useState } from "react"
import { Terminal as Xterm } from "@xterm/xterm"
import { FitAddon } from "@xterm/addon-fit"
import "@xterm/xterm/css/xterm.css"
import { api, Callout, segment } from "./common"

export default function Terminal({ execId }: { execId: string }) {
  const host = useRef<HTMLDivElement>(null)
  const retry = useRef<() => void>(() => {})
  const [message, setMessage] = useState("Connecting")
  const [error, setError] = useState<unknown>()
  useEffect(() => {
    if (!host.current) return
    const root = host.current
    const term = new Xterm({
      fontSize: 13, cursorBlink: true, convertEol: false, scrollback: 3000,
      theme: { background: "#101317", foreground: "#e2e5ea", cursor: "#99e2c5" }
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(root)
    const observer = new ResizeObserver(() => fit.fit())
    observer.observe(root)
    fit.fit()
    const abort = new AbortController()
    const path = "execs/" + segment(execId)
    let socket: WebSocket | undefined
    let cursor = 0
    let seq = 0
    let ready = false
    let exited = false
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
      setError(undefined)
      setMessage("Connecting")
      try {
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
  }, [execId])
  return <div className="terminal">
    <div className="terminal-status"><code>{execId}</code><span>{message}</span>{message === "Disconnected" && <button onClick={() => retry.current()}>Reconnect</button>}</div>
    <Callout error={error} />
    <div className="terminal-screen" ref={host} />
  </div>
}
