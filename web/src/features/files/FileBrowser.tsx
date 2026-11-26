import { useEffect, useRef, useState } from "react"
import { ArrowUp, Download, FileText, FolderClosed, Pencil, RefreshCw, Upload } from "lucide-react"
import type { GuestFileCapability, GuestFileEntry, Machine, Page } from "../../../../sdk/typescript/src/index"
import { machine as machinePath, PlatformError } from "../../../../sdk/typescript/src/index"
import { api } from "../../api"
import { Callout, download, label, machinePowerState, str } from "../../common"
import type { Actions } from "../../main"
import { Button, Modal } from "../../ui"
import { fileDigest } from "./digest"
import styles from "./files.module.css"

type Transfer = { mode: "upload" } | { mode: "preview"; path: string; contents: string; editable: boolean }
type Verification = { path: string; sha256: string }
const sizeFormat = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 })

function bytes(value: number) {
  if (value < 1024) return sizeFormat.format(value) + " B"
  if (value < 1048576) return sizeFormat.format(value / 1024) + " KiB"
  return sizeFormat.format(value / 1048576) + " MiB"
}

function fileSupport(machine: Machine, key: GuestFileCapability) {
  return machine.capabilities?.[key]
}
function fileLimit(machine: Machine, key: GuestFileCapability) {
  const cap = fileSupport(machine, key)
  const max = cap?.constraints?.max_bytes
  return cap?.supported && typeof max === "number" && Number.isSafeInteger(max) && max > 0 ? max : undefined
}
function execution(machine: Machine) {
  return JSON.stringify([machine.machine_id, machine.binding_generation, machine.binding?.binding_id,
    machine.binding?.incarnation_id, machine.binding?.execution_epoch])
}
function directory(entry: GuestFileEntry) {
  return entry.type === "directory" || entry.type === "dir"
}
function file(entry: GuestFileEntry) {
  return entry.type === "file" || entry.type === "regular"
}
const safePath = (path: string) => path.startsWith("/") && !path.includes("\0") &&
  (path === "/" || !path.slice(1).split("/").some(part => !part || part === "." || part === "..")) &&
  new TextEncoder().encode(path).length <= 4096
const child = (path: string, name: string) => path.replace(/\/$/, "") + "/" + name
const parent = (path: string) => path.slice(0, path.lastIndexOf("/")) || "/"

function changedExecution() {
  return new PlatformError(409, {
    code: "GENERATION_CONFLICT", message: "The machine execution has changed. Reopen the directory before using files.",
  })
}

async function readText(response: Response, max: number) {
  if (!response.body) return ""
  const reader = response.body.getReader()
  const decoder = new TextDecoder("utf-8", { fatal: true })
  let total = 0
  let content = ""
  try {
    for (;;) {
      const chunk = await reader.read()
      if (chunk.done) break
      total += chunk.value.byteLength
      if (total > max) throw new Error("This file exceeds the " + bytes(max) + " text preview limit.")
      content += decoder.decode(chunk.value, { stream: true })
    }
    content += decoder.decode()
    if (Array.from(content).some(char => char.charCodeAt(0) < 32 && !"\t\r\n".includes(char))) {
      throw new Error("This file contains binary data. Download it to inspect its contents.")
    }
    return content
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}

export function Files({ machine, actions }: { machine: Machine; actions: Actions }) {
  const current = execution(machine)
  const initial = useRef(current)
  return <Browser key={current} machine={machine} actions={actions} current={current} autoload={initial.current === current} />
}

function Browser({ machine, actions, current, autoload }: {
  machine: Machine; actions: Actions; current: string; autoload: boolean
}) {
  const capability = fileSupport(machine, "files.list")
  const canList = capability?.supported === true
  const canDownload = fileSupport(machine, "files.read")?.supported === true
  const downloadLimit = fileLimit(machine, "files.read")
  const uploadLimit = fileLimit(machine, "files.write")
  const previewLimit = fileLimit(machine, "files.preview")
  const editLimit = fileLimit(machine, "files.edit")
  const base = machinePath(machine.machine_id) + "/files"
  const initialPath = machine.boot_mode === "system" ? "/" : "/workspace"
  const canAutoload = useRef(autoload && canList && machinePowerState(machine) === "running")
  const [path, setPath] = useState(initialPath)
  const [input, setInput] = useState(initialPath)
  const [result, setResult] = useState<Page<GuestFileEntry>>()
  const [selected, setSelected] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [transfer, setTransfer] = useState<Transfer>()
  const [transferError, setTransferError] = useState<unknown>()
  const [notice, setNotice] = useState("")
  const [uncertain, setUncertain] = useState<Verification>()
  const active = useRef<AbortController | undefined>(undefined)
  const entry = result?.items.find(item => item.name === selected)

  async function request(source: string, signal: AbortSignal) {
    const latest = await api.getMachine(source, signal)
    if (execution(latest) !== current) throw changedExecution()
    return latest
  }

  async function useFile(action: (signal: AbortSignal) => Promise<void>) {
    if (active.current) return
    const controller = new AbortController()
    active.current = controller
    setBusy(true)
    setError(undefined)
    try {
      await action(controller.signal)
    } catch (error) {
      if (!controller.signal.aborted) setError(error)
    } finally {
      if (active.current === controller) {
        active.current = undefined
        if (!controller.signal.aborted) setBusy(false)
      }
    }
  }

  function browse(target: string, cursor?: string, initial = false) {
    if (!canList) return
    void useFile(async signal => {
      if (!safePath(target)) throw new Error("Enter an absolute Guest path without empty or relative segments.")
      if (!initial) await request(machine.machine_id, signal)
      if (!cursor) {
        setPath(target)
        setInput(target)
        setResult(undefined)
        setSelected(undefined)
      }
      const search = new URLSearchParams({ path: target, limit: "100" })
      if (cursor) search.set("cursor", cursor)
      const page = await api.get<Page<GuestFileEntry>>(base + "/list?" + search, signal)
      if (signal.aborted) return
      if (page.next_cursor && page.next_cursor === cursor) throw new Error("The server did not advance the file cursor.")
      setResult(before => ({ ...page, items: cursor ? [...before?.items ?? [], ...page.items] : page.items }))
    })
  }

  useEffect(() => {
    if (canAutoload.current) browse(initialPath, undefined, true)
    return () => {
      active.current?.abort()
      active.current = undefined
    }
  }, [])

  function open(item: GuestFileEntry) {
    if (!file(item) || previewLimit === undefined || item.size > previewLimit) return
    const target = child(path, item.name)
    void useFile(async signal => {
      await request(machine.machine_id, signal)
      const response = await api.downloadFile(machine.machine_id, target, signal)
      const contents = await readText(response, previewLimit)
      if (signal.aborted) return
      setTransferError(undefined)
      setTransfer({ mode: "preview", path: target, contents, editable: editLimit !== undefined && item.size <= editLimit })
    })
  }

  function getDownload(item: GuestFileEntry) {
    if (!file(item) || !canDownload || downloadLimit !== undefined && item.size > downloadLimit) return
    const target = child(path, item.name)
    void useFile(async signal => {
      await request(machine.machine_id, signal)
      const response = await api.downloadFile(machine.machine_id, target, signal)
      await download(response, item.name)
    })
  }

  function verify() {
    if (!uncertain || !canDownload) return
    void useFile(async signal => {
      await request(machine.machine_id, signal)
      const response = await api.downloadFile(machine.machine_id, uncertain.path, signal)
      const sha256 = await fileDigest(response.body ?? new Blob())
      if (signal.aborted) return
      setNotice(sha256 === uncertain.sha256
        ? "The Guest has the submitted bytes at " + uncertain.path + "."
        : "The current bytes differ from the submitted bytes at " + uncertain.path + ".")
      setUncertain(undefined)
    })
  }

  function submit(form: FormData) {
    if (!transfer || uncertain || active.current) return
    setTransferError(undefined)
    const preview = transfer.mode === "preview"
    const local = form.get("file")
    const payload = preview ? new Blob([str(form, "contents")]) : local instanceof File && local.name ? local : undefined
    const target = preview ? transfer.path : child(path, str(form, "name") || (local instanceof File ? local.name : ""))
    const limit = preview ? editLimit : uploadLimit
    void useFile(async signal => {
      if (!payload || limit === undefined || !safePath(target) || target === "/" ||
          !preview && parent(target) !== path) {
        setTransferError(new Error("Choose a file and a name in the current Guest directory."))
        return
      }
      if (payload.size > limit) {
        setTransferError(new Error("The maximum size is " + bytes(limit) + "."))
        return
      }
      let digest: string | undefined
      let dispatched = false
      try {
        digest = await fileDigest(payload)
        const latest = await request(machine.machine_id, signal)
        const intent = await api.intent(signal)
        dispatched = true
        const receipt = await actions.tracked(intent => api.uploadFile(machine.machine_id, target, payload, {
          revision: latest.revision, intent, signal,
        }), { intent })
        if (receipt.state !== "uploaded" || receipt.sha256 !== digest || receipt.path !== target || receipt.size !== payload.size) {
          throw new Error("The upload receipt did not match the submitted file.")
        }
        if (signal.aborted) return
        setTransfer(undefined)
        setNotice("Saved " + target)
        active.current = undefined
        browse(path)
      } catch (error) {
        if (signal.aborted) return
        const unknown = dispatched && (!(error instanceof PlatformError) ||
          error.detail.code === "OUTCOME_UNKNOWN" || error.detail.effect === "unknown")
        if (unknown && digest) {
          setUncertain({ path: target, sha256: digest })
          setTransfer(undefined)
          setNotice("The upload outcome is unknown. Verify the destination before writing again.")
          setError(error)
        } else {
          setTransferError(error)
        }
      }
    })
  }

  const selectedFile = entry && file(entry) ? entry : undefined
  const mayPreview = selectedFile && previewLimit !== undefined && selectedFile.size <= previewLimit
  const mayDownload = selectedFile && canDownload && (downloadLimit === undefined || selectedFile.size <= downloadLimit)
  const transferTitle = transfer?.mode === "preview" ? transfer.editable ? "Edit text file" : "Preview text file" : "Upload file"

  return <section className={styles.files} aria-labelledby="guest-files-title">
    <header className={styles.header}>
      <div><h2 id="guest-files-title">Guest files</h2><p>{capability?.constraints?.transport ? label(String(capability.constraints.transport)) : "Browse the Guest filesystem"}</p></div>
      <div className={styles.actions}>
        <Button size="sm" disabled={!result || busy || !mayPreview}
          title={selectedFile && previewLimit ? "Text preview limit: " + bytes(previewLimit) : undefined}
          onClick={() => selectedFile && open(selectedFile)}><Pencil size={15}/> Text</Button>
        <Button size="sm" disabled={!result || busy || !mayDownload}
          title={selectedFile && downloadLimit ? "Download limit: " + bytes(downloadLimit) : undefined}
          onClick={() => selectedFile && getDownload(selectedFile)}><Download size={15}/> Download</Button>
        <Button size="sm" variant="primary" disabled={!result || uploadLimit === undefined || busy || Boolean(uncertain)}
          onClick={() => { setTransferError(undefined); setTransfer({ mode: "upload" }) }}><Upload size={15}/> Upload</Button>
      </div>
    </header>
    <Callout error={error} />
    {notice && <div className={styles.notice} role="status">{notice} {uncertain && canDownload &&
      <Button size="sm" disabled={busy} onClick={verify}>Verify saved bytes</Button>}</div>}
    <div className={styles.browser}>
      <form className={styles.pathBar} onSubmit={event => { event.preventDefault(); browse(input.trim()) }}>
        <Button size="icon" aria-label="Parent directory" disabled={!canList || busy || path === "/"} onClick={() => browse(parent(path))}><ArrowUp size={15}/></Button>
        <input aria-label="Guest directory" value={input} onChange={event => setInput(event.target.value)} disabled={!canList} required />
        <Button type="submit" size="sm" disabled={!canList || busy}>Go</Button>
        <Button size="icon" aria-label="Reload directory" disabled={!canList || busy} onClick={() => browse(path)}><RefreshCw size={15}/></Button>
      </form>
      <div className={styles.tableScroll}>
        <table className={styles.table}>
          <thead><tr><th scope="col">Name</th><th scope="col">Size</th><th scope="col">Type</th><th scope="col">Permissions</th><th scope="col">Modified</th></tr></thead>
          <tbody>{result?.items.map(item => {
            const Icon = directory(item) ? FolderClosed : FileText
            return <tr key={item.name} data-selected={selected === item.name} onDoubleClick={() => {
              if (directory(item)) browse(child(path, item.name))
              else open(item)
            }}>
              <td><button type="button" className={styles.entry} title={item.name} aria-pressed={selected === item.name}
                onClick={() => setSelected(item.name)} onKeyDown={event => {
                  if (event.key !== "Enter") return
                  event.preventDefault()
                  if (directory(item)) browse(child(path, item.name))
                  else { setSelected(item.name); open(item) }
                }}><Icon size={16}/><span>{item.name}</span></button></td>
              <td>{directory(item) ? "—" : bytes(item.size)}</td><td>{label(item.type)}</td><td>—</td><td>—</td>
            </tr>
          })}</tbody>
        </table>
        {busy && !result && <p className={styles.empty} role="status">Reading Guest directory…</p>}
        {!busy && !result && !error && <div className={styles.empty}>
          <p>{canList ? autoload ? "Browse the Guest to open a directory." : "The execution changed. Reopen the directory to continue."
            : capability?.message ?? (capability?.status === "unsupported" ? "File access is unsupported for this machine." : "File access has not been verified for this machine.")}</p>
          {canList && <Button size="sm" onClick={() => browse(path)}>Browse directory</Button>}
        </div>}
        {result?.items.length === 0 && <p className={styles.empty}>This directory is empty.</p>}
      </div>
      <footer className={styles.footer}>
        <code>{path}</code>
        <div className={styles.actions}>
          {entry && directory(entry) && <Button size="sm" disabled={busy} onClick={() => browse(child(path, entry.name))}>Open directory</Button>}
          {result?.next_cursor && <Button size="sm" disabled={busy} onClick={() => browse(path, result.next_cursor)}>Load more</Button>}
        </div>
      </footer>
    </div>
    <Modal open={Boolean(transfer)} onOpenChange={open => { if (!open) setTransfer(undefined) }} title={transferTitle}
      description={transfer?.mode === "preview" ? transfer.path : uploadLimit ? "Maximum upload size " + bytes(uploadLimit) : undefined}
      busy={busy} size="lg">
      <form className={styles.transfer} onSubmit={event => { event.preventDefault(); submit(new FormData(event.currentTarget)) }}>
        {transfer?.mode === "preview" ? <label className="field"><span>File contents</span>
          <textarea name="contents" defaultValue={transfer.contents} readOnly={!transfer.editable} rows={12} />
        </label> : <>
          <label className="field"><span>Local file</span><input type="file" name="file" required /></label>
          <label className="field"><span>Guest filename</span><input name="name" placeholder="Use the local filename" /></label>
        </>}
        <Callout error={transferError} />
        <div className={styles.actions}>
          <Button disabled={busy} onClick={() => setTransfer(undefined)}>{transfer?.mode === "preview" && !transfer.editable ? "Close" : "Cancel"}</Button>
          {(transfer?.mode === "upload" || transfer?.editable) && <Button type="submit" variant="primary" disabled={busy || Boolean(uncertain)}>
            {busy ? "Saving…" : transfer.mode === "upload" ? "Upload" : "Save"}
          </Button>}
        </div>
      </form>
    </Modal>
  </section>
}
