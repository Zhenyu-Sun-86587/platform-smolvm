import { useState } from "react"
import type { Operation, Page } from "../../sdk/typescript/src/index"
import { operation } from "../../sdk/typescript/src/index"
import { api, Callout, Details, label, num, Records, Section, segment, State, str, useRemote } from "./common"
import type { Actions } from "./main"

export type SystemTab = "operations" | "settings" | "diagnostics" | "templates"
type Props = { tab: SystemTab; id?: string; actions: Actions; revision: number; operationIds: string[] }

export function System({ tab, ...props }: Props) {
  return <><header className="page-head"><h1>{label(tab)}</h1><button onClick={props.actions.refresh}>Refresh</button></header>
    {tab === "operations" ? <Operations {...props} /> : tab === "templates" ? <Templates {...props} />
      : tab === "diagnostics" ? <Diagnostics revision={props.revision} /> : <Settings {...props} />}</>
}

function Operations({ actions, revision, id, operationIds }: Omit<Props, "tab">) {
  const [lookup, setLookup] = useState(id ?? "")
  const op = useRemote<Operation>(id ? operation(id) : "", revision)
  const all = useRemote<Page<Operation>>("operations?limit=60", revision)
  return <>
    <Section title="Operation lookup"><form className="inline-form" onSubmit={event => {
      event.preventDefault()
      location.hash = "/operations/" + segment(lookup.trim())
    }}><label htmlFor="operation-id">Operation ID</label><input id="operation-id" value={lookup} onChange={event => setLookup(event.target.value)} required /><button>Find</button></form>
      <Callout error={op.error} />
      {op.data && <OperationDetail item={op.data} actions={actions} />}
    </Section>
    <Section title="Operation history">
      {all.error && operationIds.length === 0 ? <Callout error={all.error} /> : null}
      <div className="rows">{all.data ? all.data.items.map(item => <div key={item.operation_id} className="row">
        <a href={"#/operations/" + segment(item.operation_id)}>{item.operation_id}</a><strong>{label(item.kind)}</strong><State value={item.state} /><span>{item.stage}</span>
      </div>) : operationIds.map(item => <div key={item} className="row"><a href={"#/operations/" + segment(item)}>{item}</a></div>)}</div>
      {!all.data?.items.length && !operationIds.length && <p className="empty">No recent operations returned.</p>}
    </Section>
  </>
}

function OperationDetail({ item, actions }: { item: Operation; actions: Actions }) {
  return <>
    <Details value={{ operation: item.operation_id, kind: item.kind, stage: item.stage, state: item.state, machine_id: item.machine_id, generation: item.binding_generation, effect: item.effect }} />
    {item.error && <div className="callout" role="alert"><strong>{item.error.code}</strong><span>{item.error.message}</span><span>Effect {item.error.effect ?? item.effect}</span>{item.error.retry && <span>Retry {label(item.error.retry)}</span>}</div>}
    {item.result != null && <Records value={item.result} />}
    <div className="panel-footer actions">
      <button onClick={() => actions.dialog({
        title: "Reconcile operation", description: "Observe the current state and resume reconciliation for this operation.",
        act: () => actions.mutate(operation(item.operation_id) + "/reconcile")
      })}>Reconcile</button>
      {item.state === "queued" && <button onClick={() => actions.dialog({
        title: "Cancel queued operation", destructive: true,
        act: () => actions.mutate(operation(item.operation_id) + "/cancel")
      })}>Cancel operation</button>}
    </div>
  </>
}

function Diagnostics({ revision }: { revision: number }) {
  const status = useRemote<Record<string, unknown>>("control", revision)
  const metrics = useRemote<Record<string, unknown>>("system/metrics", revision)
  const diagnostic = useRemote<Record<string, unknown>>("system/diagnostics", revision)
  const [machineId, setMachineId] = useState("")
  const [operationId, setOperationId] = useState("")
  const [filter, setFilter] = useState("")
  const focused = useRemote<Record<string, unknown>>(filter ? "system/diagnostics?" + filter : "", revision)
  const { csrf_token: _secret, ...details } = status.data ?? {}
  return <>
    <Section title="Control status"><Callout error={status.error} />{status.data && <Details value={details} />}</Section>
    <Section title="Node metrics"><Callout error={metrics.error} />{metrics.data && <Details value={metrics.data} />}</Section>
    <Section title="Diagnostics">
      <form className="inline-form" onSubmit={event => {
        event.preventDefault()
        const filters = new URLSearchParams()
        if (machineId) filters.set("machine_id", machineId)
        if (operationId) filters.set("operation_id", operationId)
        setFilter(filters.toString())
      }}><input aria-label="Machine ID filter" placeholder="Machine ID" value={machineId} onChange={event => setMachineId(event.target.value)} />
        <input aria-label="Operation ID filter" placeholder="Operation ID" value={operationId} onChange={event => setOperationId(event.target.value)} /><button>Filter</button>
      </form>
      <Callout error={filter ? focused.error : diagnostic.error} />
      <Records value={filter ? focused.data : diagnostic.data} />
    </Section>
  </>
}

type Template = { id?: string; template_id?: string; name: string; version: number; image_digest: string; guest_release?: string; [key: string]: unknown }
function Templates({ actions, revision }: Omit<Props, "tab">) {
  const list = useRemote<Page<Template>>("templates?limit=100", revision)
  return <Section title="Template versions" actions={<button className="primary" onClick={() => actions.dialog({
    title: "Register template",
    fields: [
      { name: "template_id", label: "Template ID", required: true },
      { name: "name", label: "Name", required: true },
      { name: "version", label: "Version", type: "number", min: 1, value: 1, required: true },
      { name: "image_digest", label: "OCI image reference with digest", required: true }
    ],
    act: form => actions.mutate("templates", {
      template_id: str(form, "template_id"), name: str(form, "name"), version: num(form, "version"), image_digest: str(form, "image_digest")
    })
  })}>Register template</button>}>
    <Callout error={list.error} />
    {!list.data?.items.length && <p className="empty">No templates returned.</p>}
    <div className="rows">{list.data?.items.map(item => <div key={item.template_id ?? item.id} className="service-row">
      <div className="row"><strong>{item.name}</strong><span>Version {item.version}</span><code>{item.template_id ?? item.id}</code></div><Details value={item} />
    </div>)}</div>
  </Section>
}

type Credential = { id?: string; token_id?: string; credential_id?: string; ssh_key_id?: string; revision?: number; label?: string; scope?: unknown; revoked_at?: string; created_at?: string; [key: string]: unknown }
function Settings({ actions, revision }: Omit<Props, "tab">) {
  const settings = useRemote<Record<string, unknown>>("system/settings", revision)
  const tokens = useRemote<Page<Credential>>("tokens", revision)
  const keys = useRemote<Page<Credential>>("ssh-keys", revision)
  const certificates = useRemote<Page<Credential>>("client-certificates", revision)
  const [secret, setSecret] = useState<unknown>()
  const credentialRows = (items: Credential[] | undefined, path: string) => items?.map(item => {
    const id = item.id ?? item.credential_id ?? item.token_id ?? item.ssh_key_id
    return <div key={id} className="row"><strong>{item.label ?? id}</strong><code>{id}</code><span>{item.scope ? JSON.stringify(item.scope) : ""}</span>
      {id && !item.revoked_at && <button className="quiet-danger" onClick={() => actions.dialog({
        title: path === "tokens" ? "Revoke token" : path === "ssh-keys" ? "Remove SSH key" : "Revoke certificate", destructive: true,
        act: () => actions.mutate(path + "/" + segment(id), {}, { method: "DELETE", revision: item.revision })
      })}>{path === "ssh-keys" ? "Remove" : "Revoke"}</button>}
    </div>
  })
  return <>
    {secret != null && <Section title="Issued credential" actions={<button onClick={() => setSecret(undefined)}>Dismiss</button>}><Records value={secret} /></Section>}
    <Section title="Platform settings" actions={<button disabled={!settings.data} onClick={() => actions.dialog({
      title: "Update settings", submit: "Save",
      fields: [{ name: "default_idle_seconds", label: "Default idle timeout (seconds)", type: "number", min: 1,
        value: typeof settings.data?.default_idle_seconds === "number" ? settings.data.default_idle_seconds : undefined, required: true }],
      act: form => actions.mutate("system/settings", { default_idle_seconds: num(form, "default_idle_seconds") }, {
        method: "PATCH", revision: typeof settings.data?.revision === "number" ? settings.data.revision : undefined
      })
    })}>Update</button>}>
      <Callout error={settings.error} />{settings.data && <Details value={settings.data} />}
    </Section>
    <Section title="API tokens" actions={<button onClick={() => actions.dialog({
      title: "Create API token",
      fields: [
        { name: "label", label: "Label", required: true },
        { name: "mode", label: "Scope", options: [{ value: "read" }, { value: "manage" }, { value: "tunnel" }] },
        { name: "machine_id", label: "Machine ID for tunnel scope" },
        { name: "expires_at", label: "Expiry (RFC 3339, optional)" }
      ],
      act: async form => setSecret(await actions.mutate("tokens", {
        label: str(form, "label"), scope: {
          access: str(form, "mode"), ...(str(form, "machine_id") ? { machine_id: str(form, "machine_id") } : {})
        }, ...(str(form, "expires_at") ? { expires_at: str(form, "expires_at") } : {})
      }))
    })}>Create token</button>}>
      <Callout error={tokens.error} /><div className="rows">{credentialRows(tokens.data?.items, "tokens")}</div>
    </Section>
    <Section title="SSH public keys" actions={<button onClick={() => actions.dialog({
      title: "Add SSH public key",
      fields: [{ name: "label", label: "Label", required: true }, { name: "public_key", label: "OpenSSH public key", type: "textarea", required: true }],
      act: form => actions.mutate("ssh-keys", { label: str(form, "label"), public_key: str(form, "public_key") })
    })}>Add key</button>}>
      <Callout error={keys.error} /><div className="rows">{credentialRows(keys.data?.items, "ssh-keys")}</div>
    </Section>
    <Section title="TCP client certificates" actions={<button onClick={() => actions.dialog({
      title: "Issue client certificate",
      fields: [
        { name: "csr", label: "Certificate signing request (PEM)", type: "textarea", required: true },
        { name: "service_id", label: "Service ID", required: true },
        { name: "ttl_seconds", label: "Validity (seconds)", type: "number", min: 1, value: 3600, required: true }
      ],
      act: async form => setSecret(await actions.mutate("client-certificates", {
        csr: str(form, "csr"), scope: { service_ids: [str(form, "service_id")] }, ttl_seconds: Number(str(form, "ttl_seconds"))
      }))
    })}>Issue certificate</button>}>
      <Callout error={certificates.error} /><div className="rows">{credentialRows(certificates.data?.items, "client-certificates")}</div>
    </Section>
  </>
}
