import { Archive, Building2, FileClock, RefreshCw, Settings2, Stethoscope, Layers3 } from "lucide-react"
import { useEffect, useState } from "react"
import { Button, PageHeader } from "./ui"
import { ApiKeys } from "./features/settings/ApiKeys"
import { OrganizationPreview, PreviewPage } from "./features/settings/Previews"
import styles from "./features/settings/settings.module.css"
import catalog from "./features/catalog/catalog.module.css"
import type { BackupPolicy, BackupRun, LifecyclePolicy, Machine, NetworkDefaults, Operation, Page, PolicyApplication, PrivateNetwork, RuntimeProfile, SystemMetrics, Worker } from "../../sdk/typescript/src/index"
import { policyFields, policyInput } from "./lifecycle-policy"
import { operation } from "../../sdk/typescript/src/index"
import { api, Callout, Details, label, networkModes, num, Records, Section, segment, State, str, time, useRemote } from "./common"
import { fixtureScene, metricAvailability, metricNumber, useCollection } from "./api"
import type { Actions } from "./main"
import { memoryForkBackendLabel, memoryForkProfileLabel } from "./memory-fork"

export type SystemTab = "operations" | "settings" | "diagnostics" | "templates" | "backups" | "api-keys"
  | "agents" | "integrations" | "domains" | "environment"
type Props = { tab: SystemTab; id?: string; actions: Actions; revision: number; operationIds: string[]; create?: boolean }

const systemPages = {
  operations: { title: "Operations", Icon: FileClock, description: "Inspect platform operations, results, and recovery." },
  settings: { title: "Settings", Icon: Settings2, description: "Manage credentials and defaults for this deployment." },
  diagnostics: { title: "Diagnostics", Icon: Stethoscope, description: "Inspect control status, runtime profiles, and observations." },
  templates: { title: "Templates", Icon: Layers3, description: "Manage the template versions available for new machines." },
  backups: { title: "Backups", Icon: Archive, description: "Review backup policy schedules and recent backup runs." },
}
const settingsSections = [["settings", "General"], ["templates", "Templates"], ["backups", "Backups"], ["diagnostics", "Diagnostics"], ["api-keys", "API keys"]]

export function System({ tab, ...props }: Props) {
  if (tab === "api-keys") return <ApiKeys actions={props.actions} revision={props.revision} create={props.create} />
  if (tab === "agents" || tab === "integrations" || tab === "domains" || tab === "environment") return <PreviewPage key={tab} page={tab} />
  if (tab === "settings" && (fixtureScene === "13" || fixtureScene === "18")) return <OrganizationPreview tab={props.id} />
  const { title, Icon, description } = systemPages[tab]
  return <>
    <PageHeader title={title} icon={tab === "settings" ? <Building2 /> : <Icon />} description={description}
      actions={<Button variant="secondary" onClick={props.actions.refresh}><RefreshCw size={14} /> Refresh</Button>} />
    {tab !== "operations" && <nav className={styles.tabs} aria-label="Settings sections">
      {settingsSections.map(([key, title]) => <a key={key} aria-current={tab === key ? "page" : undefined} href={"#/" + key}>{title}</a>)}
    </nav>}
    {tab === "operations" ? <Operations {...props} /> : tab === "templates" ? <Templates {...props} />
      : tab === "diagnostics" ? <Diagnostics revision={props.revision} /> : tab === "backups" ? <Backups {...props} /> : <Settings {...props} />}
  </>
}

function Workers({ actions, revision }: Pick<Props, "actions" | "revision">) {
  const workers = useCollection<Worker>("workers", revision)
  return <Section title="Workers">
    <Callout error={workers.error} />
    <div className="rows">{workers.items?.map(worker => <div key={worker.worker_id} className="row">
      <strong>{worker.name}</strong><code>{worker.worker_id}</code>
      <span>{label(worker.observed_status)}</span><span>{label(worker.maintenance.phase)}</span>
      <span>{worker.fresh_budget ? "Fresh budget" : "No fresh budget"}</span>
      {worker.sample && <details className="disclosure">
        <summary>Worker allocation</summary>
        <dl>
          <dt>vCPU</dt><dd>{worker.sample.budget.allocated.vcpus} / {worker.sample.budget.capacity.vcpus}</dd>
          <dt>Memory</dt><dd>{worker.sample.budget.allocated.memory_mib} / {worker.sample.budget.capacity.memory_mib} MiB</dd>
          <dt>Storage</dt><dd>{(worker.sample.budget.allocated.storage_bytes / 2**30).toFixed(1)} / {(worker.sample.budget.capacity.storage_bytes / 2**30).toFixed(1)} GiB</dd>
          <dt>Sample</dt><dd>{worker.sampled_at}</dd>
        </dl>
      </details>}
      <span>{worker.maintenance.coverage === "known_only" ? "Inventory covers known objects" : label(worker.maintenance.coverage)}</span>
      {!!worker.maintenance.blockers?.length && <span>{worker.maintenance.blockers.map(label).join(", ")}</span>}
      {worker.worker_id === "local-worker" ? <span>In-process runtime</span> : <Button variant="secondary" onClick={() => actions.dialog({
        title: "Worker maintenance", submit: "Apply", fields: [
          {name: "mode", label: "Desired mode", value: worker.desired_mode, options: ["active", "cordoned", "draining"].map(value => ({value, label: label(value)}))},
          {name: "reason", label: "Reason", required: true},
        ], act: form => actions.mutate("workers/" + segment(worker.worker_id) + "/maintenance",
          {mode: str(form, "mode"), reason: str(form, "reason")}, {method:"PATCH", revision:worker.revision}),
      })}>Change mode</Button>}
    </div>)}</div>
  </Section>
}

function Backups({ actions, revision }: Omit<Props,"tab">) {
  const policies = useCollection<BackupPolicy>("backups/policies", revision)
  const runs = useCollection<BackupRun>("backups", revision)
  return <>
    <Section title="Backup policies"><Callout error={policies.error} />
      {!policies.items && !policies.error && <p role="status">Loading backup policies…</p>}
      {policies.items?.length === 0 && <p className="empty">Configure a backup policy in platformd to schedule backups.</p>}
      <div className="rows">{policies.items?.map(policy => <div className="row" key={policy.policy_id}>
        <strong>{policy.policy_id}</strong><span>{label(policy.scope)}</span><span>Every {policy.interval_seconds} seconds</span><span>Keep {policy.retain}</span>
        <Button variant="secondary" disabled={!runs.items || runs.items.some(run => run.policy_id === policy.policy_id && run.state === "running")}
          onClick={() => actions.dialog({title:"Run backup",act:() => actions.mutate("backups/policies/" + segment(policy.policy_id) + "/runs")})}>Run now</Button>
      </div>)}</div>
    </Section>
    <Section title="Backup history"><Callout error={runs.error} />
      {!runs.items && !runs.error && <p role="status">Loading backup runs…</p>}
      {runs.items?.length === 0 && <p className="empty">No backups yet.</p>}
      <div className="rows">{runs.items?.map(run => <div className="row" key={run.backup_id}>
        <strong>{run.policy_id}</strong><State value={run.state}/><span>{time(run.started_at)}</span><code>{run.backup_id}</code>
        {run.failure_code && <span>{label(run.failure_code)}</span>}
      </div>)}</div>
    </Section>
  </>
}

function Operations({ actions, revision, id, operationIds }: Omit<Props, "tab">) {
  const [lookup, setLookup] = useState(id ?? "")
  useEffect(() => setLookup(id ?? ""), [id])
  const [restores, setRestores] = useState(false)
  const op = useRemote<Operation>(id ? operation(id) : "", revision)
  const all = useCollection<Operation>(restores ? "restores" : "operations", revision)
  return <>
    <Section title="Operation lookup"><form className="inline-form" onSubmit={event => {
      event.preventDefault()
      location.hash = "/operations/" + segment(lookup.trim())
    }}><label htmlFor="operation-id">Operation ID</label><input id="operation-id" value={lookup} onChange={event => setLookup(event.target.value)} required /><button>Find</button></form>
      <Callout error={op.error} />
      {id && !op.data && !op.error && <p role="status">Loading operation…</p>}
      {op.data && <OperationDetail item={op.data} actions={actions} />}
    </Section>
    <Section title="Operation history" actions={<select aria-label="Operation history filter" value={restores ? "restores" : "all"} onChange={event => setRestores(event.target.value === "restores")}>
      <option value="all">All operations</option><option value="restores">Restores</option>
    </select>}>
      <Callout error={all.error} />
      {!all.items && !all.error && <p role="status">Loading operations…</p>}
      <div className="rows">{all.items ? all.items.map(item => <div key={item.operation_id} className="row">
        <a href={"#/operations/" + segment(item.operation_id)}>{item.operation_id}</a><strong>{label(item.kind)}</strong><State value={item.state} /><span>{item.stage}</span>
      </div>) : !restores && operationIds.map(item => <div key={item} className="row"><a href={"#/operations/" + segment(item)}>{item}</a></div>)}</div>
      {all.items && !all.items.length && <p className="empty">{restores ? "No restores returned." : "No recent operations returned."}</p>}
    </Section>
  </>
}

function OperationDetail({ item, actions }: { item: Operation; actions: Actions }) {
  const { requested_memory_fork_backend: requested, selected_memory_fork_backend: selected,
    memory_fork_profile: profile, ...otherResult } = item.result ?? {}
  const activeFork = (item.kind === "fork_batch" || item.kind === "independent_fork") &&
    (item.state === "dispatching" || item.state === "observing") && item.children?.some(child => child.state === "pending") &&
    !["fork_cancel_requested", "fork_retry", "worker_fork_retry"].includes(item.stage)
  return <>
    <Details value={{
      operation: item.operation_id, kind: item.kind, stage: item.stage, state: item.state,
      machine_id: item.machine_id, generation: item.binding_generation, worker: item.worker_id, effect: item.effect,
      source_disposition: item.source_disposition, source_lock_released: item.source_lock_released,
      ...(requested ? { requested_memory_fork_backend: memoryForkBackendLabel(requested) } : {}),
      ...(selected ? { selected_memory_fork_backend: memoryForkBackendLabel(selected) } : {}),
      ...(profile ? { memory_fork_profile: memoryForkProfileLabel(profile) } : {}),
    }} />
    {item.error && <div className="callout" role="alert"><strong>{item.error.code}</strong><span>{item.error.message}</span><span>Effect {item.error.effect ?? item.effect}</span>{item.error.retry && <span>Retry {label(item.error.retry)}</span>}</div>}
    {item.result != null && (Object.keys(otherResult).length > 0 || !(requested || selected || profile)) && <Records value={otherResult} />}
    {Boolean(item.children?.length) && <div className="rows" aria-label="Forked machines">
      {item.children?.map(child => <div className="row" key={child.ordinal}>
        {child.state === "ready" ? <a href={"#/machines/" + segment(child.machine_id)}>{child.machine_id}</a> : <code>{child.machine_id}</code>}
        <State value={child.state} />
        {child.attempt > 0 && <span>Attempt {child.attempt + 1}</span>}
        {child.resource_independence && <span>Resource independence {child.resource_independence}</span>}
        {child.error_code && <code>{child.error_code}</code>}
      </div>)}
    </div>}
    <div className="panel-footer actions">
      {item.source_lock_released && (item.state === "succeeded" || item.state === "failed") && item.children?.some(child => child.error_code === "NATIVE_CHILD_ABORTED") &&
        <button onClick={() => actions.dialog({
          title: "Retry failed fork targets",
          description: "Start a new attempt for each eligible failed target using the original captured state.",
          act: () => actions.mutate(operation(item.operation_id) + "/retry", {}, { revision: item.revision })
        })}>Retry failed targets</button>}
      <button onClick={() => actions.dialog({
        title: "Reconcile operation", description: "Observe the current state and resume reconciliation for this operation.",
        act: () => actions.mutate(operation(item.operation_id) + "/reconcile")
      })}>Reconcile</button>
      {(item.state === "queued" || activeFork) && <button onClick={() => actions.dialog({
        title: activeFork ? "Cancel pending fork targets" : "Cancel queued operation", destructive: true,
        description: activeFork ? "The Worker will cancel targets that have not committed. You can follow each target in this operation." : undefined,
        act: () => actions.mutate(operation(item.operation_id) + "/cancel")
      })}>{activeFork ? "Cancel pending targets" : "Cancel operation"}</button>}
    </div>
  </>
}

function Diagnostics({ revision }: { revision: number }) {
  const status = useRemote<Record<string, unknown>>("control", revision)
  const profiles = useCollection<RuntimeProfile>("runtime-profiles", revision)
  const metrics = useRemote<SystemMetrics>("system/metrics", revision)
  const availability = metricAvailability(metrics.data)
  const measured = (value?: number) => availability !== "unknown" && availability !== "unavailable" ? metricNumber(value) : undefined
  const diagnostic = useRemote<Record<string, unknown>>("system/diagnostics", revision)
  const [machineId, setMachineId] = useState("")
  const [operationId, setOperationId] = useState("")
  const [filter, setFilter] = useState("")
  const focused = useRemote<Record<string, unknown>>(filter ? "system/diagnostics?" + filter : "", revision)
  const { csrf_token: _secret, ...details } = status.data ?? {}
  return <>
    <Section title="Control status"><Callout error={status.error} />{status.data && <Details value={details} />}</Section>
    <Section title="Registered runtime profiles">
      <Callout error={profiles.error} />
      {profiles.items?.length === 0 && <p className="empty">No deep runtime profiles registered.</p>}
      {profiles.items?.map(profile => <details className="disclosure" key={profile.runtime_instance_id}>
        <summary>{profile.runtime_instance_id} · {profile.boot_profiles.join(", ") || label(profile.status)}</summary>
        <Details value={{
          bundle: profile.runtime_bundle_id, state_reader: profile.state_reader_id,
          verification: label(profile.verification_status), security: label(profile.security_status),
          running: profile.running_bindings, standby: profile.standby_bindings,
          hibernated: profile.hibernated_bindings, bundle_binding_references: profile.bundle_binding_refs,
          bundle_snapshot_references: profile.bundle_snapshot_refs,
        }} />
      </details>)}
    </Section>
    <Section title="Runtime metrics">
      <Callout error={metrics.error} />
      {metrics.data ? <Details value={{
        availability: label(availability),
        scope: metrics.data.scope,
        source: metrics.data.source,
        observed: time(metrics.data.observed_at),
        cpu_allocation: measured(metrics.data.allocated_cpus),
        memory_allocation_mb: measured(metrics.data.allocated_memory_mb),
        cpu_used: measured(metrics.data.used_cpus),
        memory_used_mb: measured(metrics.data.used_memory_mb),
        disk_used_gb: measured(metrics.data.used_disk_gb),
        issues: metrics.data.issues,
      }} /> : !metrics.error && <p role="status">Loading runtime metrics…</p>}
    </Section>
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
  const list = useCollection<Template>("templates", revision)
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
    {!list.items && !list.error && <p role="status">Loading templates…</p>}
    {list.items?.length === 0 && <p className="empty">No templates returned.</p>}
    <div className="rows">{list.items?.map(item => <div key={item.template_id ?? item.id} className="service-row">
      <div className="row"><strong>{item.name}</strong><span>Version {item.version}</span><code>{item.template_id ?? item.id}</code></div><Details value={item} />
    </div>)}</div>
  </Section>
}

type Credential = { id?: string; token_id?: string; credential_id?: string; ssh_key_id?: string; revision?: number; label?: string; scope?: unknown; revoked_at?: string; created_at?: string; [key: string]: unknown }
function ApplyDefaults({ actions, revision, defaultRevision }: Pick<Props, "actions" | "revision"> & { defaultRevision: number }) {
  const [open, setOpen] = useState(false)
  const [pages, setPages] = useState([""])
  const [selected, setSelected] = useState<Record<string, number>>({})
  const [receipt, setReceipt] = useState<PolicyApplication>()
  const cursor = pages[pages.length - 1]
  const machines = useRemote<Page<Machine>>(open ? "machines?limit=25&cursor=" + encodeURIComponent(cursor) : "", revision)
  const result = useRemote<PolicyApplication>(receipt ? "lifecycle-policy-applications/" + segment(receipt.application_id) : "", revision)
  const current = result.data ?? receipt
  const targets = Object.entries(selected).map(([machine_id, expected_revision]) => ({ machine_id, expected_revision }))
  return <details className="disclosure" onToggle={event => setOpen(event.currentTarget.open)}>
    <summary>Apply defaults to existing machines</summary>
    <Callout error={machines.error} />
    <div className="rows">{machines.data?.items.map(machine => <label className="row" key={machine.machine_id}>
      <input type="checkbox" checked={Object.hasOwn(selected, machine.machine_id)}
        disabled={!machine.lifecycle_policy?.supported || (!Object.hasOwn(selected, machine.machine_id) && targets.length >= 256)}
        onChange={event => {
          const add = event.currentTarget.checked
          setSelected(previous => {
            const next = { ...previous }
            if (add) next[machine.machine_id] = machine.revision
            else delete next[machine.machine_id]
            return next
          })
        }} />
      <strong>{machine.name}</strong><code>{machine.machine_id}</code>
      <span>{machine.lifecycle_policy?.supported ? "Revision " + machine.revision : "Policy unavailable"}</span>
    </label>)}</div>
    <div className="actions">
      {pages.length > 1 && <button onClick={() => setPages(pages.slice(0, -1))}>Previous</button>}
      {machines.data?.next_cursor && <button onClick={() => setPages([...pages, machines.data!.next_cursor!])}>Next</button>}
      <button disabled={targets.length === 0} onClick={() => actions.dialog({
        title: "Apply lifecycle defaults", submit: "Apply to selected machines",
        description: `Replace lifecycle overrides on ${targets.length} selected machine(s) with default revision ${defaultRevision}.`,
        act: async () => {
          const applied = await actions.mutate<PolicyApplication>("lifecycle-policy-applications", {
            default_policy_revision: defaultRevision, machines: targets,
          })
          setReceipt(applied)
          setSelected({})
        },
      })}>Apply to selected machines ({targets.length})</button>
    </div>
    {current && <div aria-live="polite">
      <p>Default revision {current.default_policy_revision} · <code>{current.application_id}</code></p>
      <Callout error={result.error} />
      <div className="rows">{current.machines.map(m => <div className="row" key={m.machine_id}>
        <code>{m.machine_id}</code><State value={m.state}/>{m.reason && <span>{label(m.reason)}</span>}
      </div>)}</div>
      {!current.complete && <button onClick={actions.refresh}>Refresh results</button>}
    </div>}
  </details>
}

function Settings({ actions, revision }: Omit<Props, "tab">) {
  const settings = useRemote<Record<string, unknown> & NetworkDefaults & { default_lifecycle_policy?: LifecyclePolicy }>("system/settings", revision)
  const networks = useCollection<PrivateNetwork>("networks", revision)
  const networkIDs = Array.from(new Set((networks.items ?? []).map(network => network.network_id)))
  const keys = useCollection<Credential>("ssh-keys", revision)
  const certificates = useCollection<Credential>("client-certificates", revision)
  const [secret, setSecret] = useState<unknown>()
  const credentialRows = (items: Credential[] | undefined, path: string) => items?.map(item => {
    const id = item.id ?? item.credential_id ?? item.token_id ?? item.ssh_key_id
    return <div key={id} className="row"><strong>{item.label ?? id}</strong><code>{id}</code><span>{item.scope ? JSON.stringify(item.scope) : ""}</span>
      {id && !item.revoked_at && <button className="quiet-danger" onClick={() => actions.dialog({
        title: path === "ssh-keys" ? "Remove SSH key" : "Revoke certificate", destructive: true,
        act: () => actions.mutate(path + "/" + segment(id), {}, { method: "DELETE", revision: item.revision })
      })}>{path === "ssh-keys" ? "Remove" : "Revoke"}</button>}
    </div>
  })
  return <>
    <Workers actions={actions} revision={revision} />
    {secret != null && <Section title="Issued credential" actions={<button onClick={() => setSecret(undefined)}>Dismiss</button>}><Records value={secret} /></Section>}
    <Section title="Platform settings" actions={<button disabled={!settings.data} onClick={() => actions.dialog({
      title: "Update settings", submit: "Save",
      fields: [{ name: "default_idle_seconds", label: "Default idle timeout (seconds)", type: "number", min: 1,
        value: typeof settings.data?.default_idle_seconds === "number" ? settings.data.default_idle_seconds : undefined, required: true },
        { name: "default_boot_mode", label: "Default boot mode", value: String(settings.data?.default_boot_mode ?? "oci"),
          options: [{value: "oci", label: "OCI"}, {value: "system", label: "System"}] }],
      act: form => actions.mutate("system/settings", { default_idle_seconds: num(form, "default_idle_seconds"), default_boot_mode: str(form, "default_boot_mode") }, {
        method: "PATCH", revision: typeof settings.data?.revision === "number" ? settings.data.revision : undefined
      })
    })}>Update</button>}>
      <Callout error={settings.error} />
      {!settings.data && !settings.error && <div className={catalog.skeleton} role="status" aria-label="Loading settings" />}
      {settings.data && <Details value={{ revision: settings.data.revision, default_boot_mode: settings.data.default_boot_mode,
        default_idle_timeout_seconds: settings.data.default_idle_seconds }} />}
    </Section>
    <Callout error={networks.error} />
    {settings.data && networkIDs.length > 0 && <Section title="Private network defaults" actions={<button onClick={() => actions.dialog({
      title: "Edit private network defaults", submit: "Save",
      fields: [
        { name: "network_id", label: "Default private network", value: settings.data?.default_network_id ?? "",
          options: [{value: "", label: "Use each runtime’s configured network"}, ...networkIDs.map(value => ({value}))] },
        { name: "network_mode", label: "Default network mode", value: settings.data?.default_network_mode, options: networkModes },
      ],
      act: form => actions.mutate("system/settings", {
        default_network_id: str(form, "network_id"), default_network_mode: str(form, "network_mode"),
      }, { method: "PATCH", revision: Number(settings.data!.revision) }),
    })}>Edit network defaults</button>}>
      <p>These defaults apply to new System machines.</p>
      <Details value={{ network: settings.data.default_network_id || "Runtime default",
        mode: networkModes.find(mode => mode.value === settings.data?.default_network_mode)?.label }} />
    </Section>}
    {settings.data?.default_lifecycle_policy && <Section title="Default lifecycle policy" actions={<button onClick={() => actions.dialog({
      title: "Edit default lifecycle policy", submit: "Save", sheet: true,
      fields: policyFields(settings.data!.default_lifecycle_policy!),
      act: form => actions.mutate("system/settings", { default_lifecycle_policy: policyInput(form) },
        { method: "PATCH", revision: Number(settings.data!.revision) }),
    })}>Edit defaults</button>}>
      <p>New machines inherit default revision {String(settings.data.default_lifecycle_revision)}.</p>
      <Details value={{ ...settings.data.default_lifecycle_policy }} />
      <ApplyDefaults actions={actions} revision={revision} defaultRevision={Number(settings.data.default_lifecycle_revision)} />
    </Section>}
    <Section title="SSH public keys" actions={<button onClick={() => actions.dialog({
      title: "Add SSH public key",
      fields: [{ name: "label", label: "Label", required: true }, { name: "public_key", label: "OpenSSH public key", type: "textarea", required: true }],
      act: form => actions.mutate("ssh-keys", { label: str(form, "label"), public_key: str(form, "public_key") })
    })}>Add key</button>}>
      <Callout error={keys.error} />
      {keys.items?.length === 0 && <p className="empty">No SSH public keys yet.</p>}
      {!keys.items && !keys.error && <p role="status">Loading SSH public keys…</p>}
      <div className="rows">{credentialRows(keys.items, "ssh-keys")}</div>
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
      <Callout error={certificates.error} />
      {certificates.items?.length === 0 && <p className="empty">No client certificates yet.</p>}
      {!certificates.items && !certificates.error && <p role="status">Loading client certificates…</p>}
      <div className="rows">{credentialRows(certificates.items, "client-certificates")}</div>
    </Section>
  </>
}
