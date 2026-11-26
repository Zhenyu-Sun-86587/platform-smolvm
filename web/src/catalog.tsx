import { useEffect, useRef, useState } from "react"
import type { FormEvent } from "react"
import * as Dropdown from "@radix-ui/react-dropdown-menu"
import type { Machine, Service, Snapshot, Task } from "../../sdk/typescript/src/index"
import { ArrowUpRight, Check, Globe2, Layers3, MapPin, Plus, Search, Upload, Zap } from "lucide-react"
import { api, collection, fixtureScene, segment, useCollection } from "./api"
import { bundleBound, Callout, fullStateBlock, label, time } from "./common"
import { capabilityReason, fileDigest, Services, ServiceStatus, Snapshots, Tasks } from "./machine"
import { machineHref } from "./routes"
import { DocumentationLink } from "./features/catalog/DocumentationLink"
import { Button, Modal, PageHeader, StatusBadge } from "./ui"
import styles from "./features/catalog/catalog.module.css"
import type { Actions } from "./main"

type PageName = "snapshots" | "tasks" | "networking"
type CatalogContext = { machines?: Machine[]; actions: Actions; revision: number }
type Row =
  | { kind: "task"; id: string; machine: string; task: Task }
  | { kind: "service"; id: string; machine: string; service: Service }

async function readRows(page: "tasks" | "networking", id: string, signal: AbortSignal): Promise<Row[]> {
  const path = "machines/" + segment(id)
  if (page === "tasks") {
    const tasks = await collection<Task>(path + "/tasks", signal)
    return tasks.flatMap(task => {
      const key = task.task_id ?? task.id
      return key ? [{ kind: "task", id: key, machine: id, task }] : []
    })
  }
  const services = await collection<Service>(path + "/services", signal)
  return services.map(service => ({ kind: "service", id: service.service_id, machine: id, service }))
}

export function Catalog({ page, ...context }: CatalogContext & { page: PageName }) {
  if (page === "snapshots") return <SnapshotCatalog {...context} />
  return <MachineCatalog page={page} {...context} />
}

function SnapshotCatalog({ machines, actions, revision }: CatalogContext) {
  const [create, setCreate] = useState(fixtureScene === "16")
  const [state, setState] = useState("all")
  const [query, setQuery] = useState("")
  const [search, setSearch] = useState(false)
  const [region, setRegion] = useState("fra")
  const snapshots = useCollection<Snapshot>("snapshots", revision)
  const current = (snapshots.items ?? []).filter(item =>
    (state === "all" || item.state === state) &&
    `${item.label ?? ""} ${item.snapshot_id} ${item.source_machine_id ?? ""}`.toLowerCase().includes(query.trim().toLowerCase())
  )
  function showImport() {
    actions.dialog({
      title: "Import snapshot",
      fields: [{ name: "archive", label: "Snapshot archive", type: "file", required: true }],
      act: async form => {
        const file = form.get("archive")
        if (!(file instanceof File) || !file.size) throw new Error("Choose a snapshot archive.")
        const sha256 = await fileDigest(file)
        return actions.tracked(intent => api.upload("snapshots/import", file, { method: "POST", sha256, intent }),
          { key: "snapshot-import:" + sha256 })
      },
    })
  }
  const demo = fixtureScene === "05" || fixtureScene === "16"
  const statuses = Array.from(new Set(["all", "ready", "pending", "failed", "archived", ...(snapshots.items ?? []).map(item => item.state)]))
  return <>
    <PageHeader title="Snapshots" icon={<Layers3 />} description={demo ? "Versioned, reusable images captured from your machines. Launch new machines from one in seconds." : "Capture machine state and restore workspaces from saved snapshots."} className={styles.stackedHeading}
      actions={<><Button className={styles.neutralPrimary} variant="primary" onClick={() => setCreate(true)} disabled={!machines?.length}><Plus size={15} /> New snapshot</Button>
        {!demo && <Button onClick={showImport}><Upload size={15} /> Import</Button>}<DocumentationLink /></>} />
    <div className={styles.toolbar}>
      <div className={styles.segments} aria-label="Snapshot status">
        {statuses.map(value => <button type="button" key={value} aria-pressed={value === state} onClick={() => setState(value)}>{label(value)}</button>)}
      </div>
      <div className={styles.actions}>
        {demo && <Dropdown.Root>
          <Dropdown.Trigger asChild><Button variant="secondary" size="icon" className={styles.regionTrigger}
            aria-label="Filter by region"><MapPin size={15} /></Button></Dropdown.Trigger>
          <Dropdown.Portal><Dropdown.Content className={styles.regionMenu} align="end" sideOffset={5}>
            <Dropdown.RadioGroup value={region} onValueChange={setRegion}>
              {[["fra", "Frankfurt"], ["nyc", "New York"]].map(([value, title]) =>
                <Dropdown.RadioItem key={value} value={value}>{title}<Dropdown.ItemIndicator><Check size={14} /></Dropdown.ItemIndicator></Dropdown.RadioItem>)}
            </Dropdown.RadioGroup>
          </Dropdown.Content></Dropdown.Portal>
        </Dropdown.Root>}
        <Button variant="secondary" size="icon" aria-label="Search snapshots" aria-expanded={search} onClick={() => setSearch(!search)}><Search size={15} /></Button>
      </div>
    </div>
    {search && <div className={styles.search}><Search size={15} /><input aria-label="Find snapshots" placeholder="Search snapshots…" autoFocus
      value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => { if (event.key === "Escape") setSearch(false) }} /></div>}
    <Callout error={snapshots.error} />
    {!snapshots.items && !snapshots.error && <div className={styles.skeleton} role="status" aria-label="Loading snapshots" />}
    {snapshots.items && !current.length && <p className={styles.empty}>{demo
      ? `No snapshots in ${region === "fra" ? "Frankfurt" : "New York"}.`
      : query || state !== "all" ? "No matching snapshots." : "No snapshots yet."}</p>}
    {current.length > 0 && <Snapshots directory items={current} machines={machines} actions={actions} revision={revision} />}
    {create && <NewSnapshot machines={machines ?? []} actions={actions} close={() => setCreate(false)} />}
  </>
}

function NewSnapshot({ machines, actions, close }: { machines: Machine[]; actions: Actions; close: () => void }) {
  const [source, setSource] = useState("")
  const [name, setName] = useState("")
  const [mode, setMode] = useState("")
  const [busy, setBusy] = useState(false)
  const inFlight = useRef(false)
  const [error, setError] = useState<unknown>()
  const machine = machines.find(machine => machine.machine_id === source)
  const can = {
    clone: machine && !capabilityReason(machine, "clean_checkpoint"),
    recovery: machine && !capabilityReason(machine, "active_recovery"),
    vm: machine && bundleBound(machine) && !fullStateBlock(machine, "state.checkpoint"),
  }
  const options = [
    { value: "clone", label: "Clean snapshot", available: can.clone },
    { value: "recovery", label: "Recovery snapshot", available: can.recovery },
    { value: "vm", label: "VM checkpoint", available: can.vm },
  ].filter(option => option.available)
  const selected = options.some(option => option.value === mode) ? mode : options[0]?.value
  const capture = Boolean(name.trim() && selected && machine && !machine.operation_id)

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (inFlight.current || !capture || !machine) return
    inFlight.current = true
    setBusy(true)
    setError(undefined)
    try {
      await actions.mutate(`machines/${segment(machine.machine_id)}/snapshots`, {
        label: name.trim(),
        ...(selected === "vm" ? { capture_mode: "vm" } : { purpose: selected }),
        expected_generation: machine.binding_generation,
      }, { revision: machine.revision })
      close()
    } catch (reason) {
      setError(reason)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  return <Modal open busy={busy} onOpenChange={open => { if (!open) close() }} title="New snapshot" className={styles.smallModal}
    description="Capture a machine’s current state as a reusable snapshot."
    footer={<><Button variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
      <Button variant="primary" type="submit" form="capture-snapshot" disabled={busy || !capture}>{busy ? "Capturing…" : "Capture"}</Button></>}>
    <form id="capture-snapshot" className={styles.form} onSubmit={submit}>
      <label>Machine<select aria-label="Machine" value={source} disabled={busy} onChange={event => { setSource(event.target.value); setMode("") }}>
        <option value="">Select a machine…</option>
        {machines.map(item => <option key={item.machine_id} value={item.machine_id}>{item.name}</option>)}
      </select></label>
      <label>Snapshot name<input value={name} placeholder="e.g. base-image" disabled={busy} onChange={event => setName(event.target.value)} /></label>
      {options.length > 1 && <label>Capture type<select aria-label="Capture type" value={selected} disabled={busy} onChange={event => setMode(event.target.value)}>
        {options.map(item => <option key={item.value} value={item.value}>{item.label}</option>)}
      </select></label>}
      {selected === "vm" && <p>Capture RAM and supported disks for a System machine.</p>}
      {machine && !options.length && <p role="status">No capture mode is available for this machine.</p>}
      {machine?.operation_id && <p role="status">Wait for the active machine operation to finish.</p>}
      <Callout error={error} />
    </form>
  </Modal>
}

function MachineCatalog({ page, machines, actions, revision }: CatalogContext & { page: "tasks" | "networking" }) {
  const [selected, setSelected] = useState("")
  const [list, setList] = useState<{ key: string; rows: Row[]; errors: { id: string; error: unknown }[] }>()
  const machine = machines?.find(item => item.machine_id === selected)
  const keys = machines ? JSON.stringify(machines.map(item => item.machine_id)) : undefined
  const key = keys && JSON.stringify([page, keys])
  const loaded = list?.key === key ? list : undefined
  const taskPage = page === "tasks"
  const automationPreview = taskPage && fixtureScene === "07"
  const machineNames = new Map(machines?.map(item => [item.machine_id, item]))

  useEffect(() => {
    if (!keys || !key || machine) return
    const ids: string[] = JSON.parse(keys)
    const abort = new AbortController()
    async function load() {
      const rows: Row[] = []
      const errors: { id: string; error: unknown }[] = []
      for (let i = 0; i < ids.length && !abort.signal.aborted; i += 5) {
        const batch = ids.slice(i, i + 5)
        const results = await Promise.allSettled(batch.map(id => readRows(page, id, abort.signal)))
        results.forEach((result, j) => {
          if (result.status === "fulfilled") rows.push(...result.value)
          else errors.push({ id: batch[j], error: result.reason })
        })
      }
      if (!abort.signal.aborted) setList({ key: key!, rows, errors })
    }
    void load()
    return () => abort.abort()
  }, [keys, key, page, machine?.machine_id, revision])

  return <>
    <PageHeader title={taskPage ? "Automations" : "Services"} icon={taskPage ? <Zap /> : <Globe2 />}
      description={automationPreview ? "Scripts running on your machines, driven by schedules and events."
        : taskPage ? "View persistent guest tasks and their execution status." : "Manage published guest ports and connection details."}
      actions={<>{machines?.length && !automationPreview ? <select className={styles.scope} aria-label="Machine scope" value={machine?.machine_id ?? ""} onChange={event => setSelected(event.target.value)}>
        <option value="">All machines</option>
        {machines.map(item => <option key={item.machine_id} value={item.machine_id}>{item.name}</option>)}
      </select> : null}<DocumentationLink /></>} />
    {machine ? taskPage ? <Tasks key={machine.machine_id} machine={machine} actions={actions} revision={revision} />
      : <Services key={machine.machine_id} machine={machine} actions={actions} revision={revision} />
      : <>
        {loaded?.errors.map(({ id, error }) => <div key={id} className={styles.aggregateError}>
          <strong>{machineNames.get(id)?.name ?? id}</strong><Callout error={error} />
        </div>)}
        {!loaded && machines && <div className={styles.skeleton} role="status" aria-label={taskPage ? "Loading tasks" : "Loading services"} />}
        {loaded && !loaded.rows.length && !loaded.errors.length && <section className={[styles.featurePanel, automationPreview && styles.automationPanel].filter(Boolean).join(" ")}>
          <div>
            {automationPreview && <span className={styles.panelKicker}>Defined in code</span>}
            <h2>{automationPreview ? "No automations yet" : taskPage ? "No persistent tasks yet" : "No published services yet"}</h2>
            <p>{automationPreview ? "Scheduled scripts and their execution history appear after you register a schedule or trigger."
              : taskPage ? "Choose a machine to start a task and inspect its output." : "Choose a machine to publish a guest port."}</p>
            {automationPreview && <DocumentationLink />}
            {machines?.length === 0 && <a href="#/machines">View machines <ArrowUpRight size={13} /></a>}
          </div>
        </section>}
        {loaded && loaded.rows.length > 0 && <div className={styles.tableFrame}><table className={styles.table}>
          <thead><tr>{(taskPage ? ["Task", "Machine", "State", "Last observation", "Actions"] :
            ["Service", "Machine", "Configuration", "Observation", "Endpoint", "Actions"]).map(heading => <th key={heading} scope="col">{heading}</th>)}</tr></thead>
          <tbody>{loaded.rows.map(row => {
            const target = machineNames.get(row.machine)
            const href = machineHref(row.machine, row.kind === "task" ? "tasks" : "networking")
            return <tr key={row.machine + "/" + row.id}>
              <td><a href={href}>{row.kind === "task" ? row.id : row.service.name}</a></td>
              <td>{target?.name ?? row.machine}</td>
              {row.kind === "task" ? <><td><StatusBadge value={row.task.state} /></td><td>{time(row.task.observed_at)}</td></> : <>
                <td><StatusBadge value={row.service.enabled === false ? "disabled" : "enabled"} /></td>
                <td><ServiceStatus item={row.service} machine={target} /></td>
                <td><code>{row.service.public_host ?? row.service.protocol} → :{row.service.guest_port}</code></td>
              </>}
              <td><a href={href}>{row.kind === "task" ? "View tasks" : "Manage"} <ArrowUpRight size={13} /></a></td>
            </tr>
          })}</tbody>
        </table></div>}
      </>}
    {loaded?.errors.length ? <Button className={styles.retry} onClick={actions.refresh}>Retry directory</Button> : null}
  </>
}
