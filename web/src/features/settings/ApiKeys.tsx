import { useRef, useState } from "react"
import type { FormEvent } from "react"
import { Copy, KeyRound, Plus } from "lucide-react"
import type { Token, TokenSpec } from "../../../../sdk/typescript/src/index"
import { useCollection } from "../../api"
import { Callout, label, segment, time } from "../../common"
import type { Actions } from "../../main"
import { Button, Modal, PageHeader } from "../../ui"
import catalog from "../catalog/catalog.module.css"
import styles from "./settings.module.css"

export function ApiKeys({ actions, revision, create }: { actions: Actions; revision: number; create?: boolean }) {
  const keys = useCollection<Token>("tokens", revision)
  const [newKey, setNewKey] = useState(false)
  function close() {
    setNewKey(false)
    if (new URLSearchParams(location.hash.split("?")[1]).get("action") === "new") location.hash = "/api-keys"
  }
  function revoke(token: Token) {
    actions.dialog({
      title: "Revoke API key",
      description: `Requests authenticated with ${token.label || token.token_id} will be rejected after revocation.`,
      submit: "Revoke",
      destructive: true,
      act: () => actions.mutate("tokens/" + segment(token.token_id), {}, { method: "DELETE", revision: token.revision }),
    })
  }
  return <>
    <PageHeader title="API keys" icon={<KeyRound />} description="Programmatic credentials for the platform API and CLI."
      actions={<Button variant="primary" onClick={() => setNewKey(true)}><Plus size={15} /> Create key</Button>} />
    <Callout error={keys.error} />
    {!keys.items && !keys.error && <div className={catalog.skeleton} role="status" aria-label="Loading API keys" />}
    {keys.items?.length === 0 && <section className={catalog.featurePanel} data-split="true">
      <div><h2>Create an API key</h2><p>Use a key in CI or scripts that call the platform API.</p>
        <Button variant="primary" onClick={() => setNewKey(true)}>Create key</Button></div>
      <div><span className={styles.eyebrow}>CLI</span><pre>$ sandboxctl credentials tokens create --help</pre></div>
    </section>}
    {!!keys.items?.length && <div className={catalog.tableFrame}><table className={catalog.table}>
      <thead><tr>{["Key", "Access", "Created", "Expires", "Actions"].map(column => <th key={column} scope="col">{column}</th>)}</tr></thead>
      <tbody>{keys.items.map(token => <tr key={token.token_id}>
        <td><strong>{token.label || token.token_id}</strong><small>{token.token_id}</small></td>
        <td>{label(token.scope.access)}{token.scope.machine_id && <small>{token.scope.machine_id}</small>}</td>
        <td>{time(token.created_at)}</td><td>{token.expires_at ? time(token.expires_at) : "No expiry"}</td>
        <td>{token.revoked_at ? `Revoked ${time(token.revoked_at)}` : <Button size="sm" variant="ghost"
          onClick={() => revoke(token)}>Revoke</Button>}</td>
      </tr>)}</tbody>
    </table></div>}
    {(newKey || create) && <NewKey actions={actions} close={close} />}
  </>
}

function NewKey({ actions, close }: { actions: Actions; close: () => void }) {
  const [name, setName] = useState("")
  const [access, setAccess] = useState<TokenSpec["scope"]["access"]>("read")
  const [machine, setMachine] = useState("")
  const [expiry, setExpiry] = useState("")
  const [issued, setIssued] = useState<Token>()
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState<unknown>()
  const inFlight = useRef(false)
  const expiryDate = !!expiry && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(expiry) && Date.parse(expiry)
  const validMachine = /^m_[a-z0-9_-]{1,62}$/.test(machine)
  const valid = Boolean(name.trim()) && new TextEncoder().encode(name).length <= 120 &&
    (machine ? validMachine : access !== "tunnel") && (!expiry || (typeof expiryDate === "number" && expiryDate > Date.now()))
  const title = issued ? "API key created" : "Create API key"

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (inFlight.current || !valid) return
    inFlight.current = true
    setBusy(true)
    setError(undefined)
    const request: TokenSpec = {
      label: name,
      scope: { access, ...(machine ? { machine_id: machine } : {}) },
      ...(expiry ? { expires_at: expiry } : {}),
    }
    try {
      setIssued(await actions.mutate<Token>("tokens", request))
    } catch (reason) {
      setError(reason)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  async function copy() {
    if (!issued?.token) return
    try {
      await navigator.clipboard.writeText(issued.token)
      setCopied(true)
    } catch (reason) {
      setError(reason)
    }
  }

  return <Modal open busy={busy} onOpenChange={open => { if (!open) close() }} title={title} className={catalog.smallModal}
    footer={issued ? <Button variant="primary" onClick={close}>Done</Button> : <>
      <Button variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
      <Button variant="primary" type="submit" form="api-key-form" disabled={!valid || busy}>{busy ? "Creating…" : "Create key"}</Button>
    </>}>
    {issued ? <div className={catalog.form}>
      <p>{issued.token ? "Copy this key now. Its secret is shown once." :
        "The server returned an existing key without its secret. Revoke that key to replace it."}</p>
      {issued.token && <label>API key<input readOnly autoComplete="off" spellCheck={false} value={issued.token}
        onFocus={event => event.currentTarget.select()} /></label>}
      {issued.token && <Button onClick={copy}><Copy size={14} /> {copied ? "Copied" : "Copy key"}</Button>}
      <span role="status" className="sr-only">{copied ? "API key copied" : ""}</span>
      <Callout error={error} />
    </div> : <form id="api-key-form" className={catalog.form} onSubmit={submit}>
      <label>Label<input value={name} disabled={busy} required onChange={event => setName(event.target.value)} /></label>
      <label>Access<select value={access} disabled={busy} onChange={event => {
        const value = event.target.value
        if (value === "read" || value === "manage" || value === "tunnel") setAccess(value)
      }}><option value="read">Read</option><option value="manage">Manage</option><option value="tunnel">Tunnel</option></select></label>
      <label>Machine ID{access !== "tunnel" && " (optional)"}
        <input value={machine} placeholder="m_…" disabled={busy} required={access === "tunnel"} aria-invalid={Boolean(machine && !validMachine)}
          onChange={event => setMachine(event.target.value)} /></label>
      <label>Expiry (RFC 3339, optional)<input value={expiry} placeholder="2027-01-01T00:00:00Z" disabled={busy}
        onChange={event => setExpiry(event.target.value)} /></label>
      <Callout error={error} />
    </form>}
  </Modal>
}
