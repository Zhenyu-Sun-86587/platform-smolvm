import { useState } from "react"
import type { ReactNode } from "react"
import * as Dropdown from "@radix-ui/react-dropdown-menu"
import {
  Activity, ArrowUpRight, Check, ChevronLeft, ChevronRight, Cpu, HardDrive,
  GitFork, Layers3, MapPin, MemoryStick, Plus, Search, Server, Smartphone, Terminal, Users, X,
} from "lucide-react"
import type { BootMode, Machine, MachineMetrics, Operation, Page, SystemMetrics } from "../../sdk/typescript/src/index"
import { fixtureScene, metricAvailability, metricNumber } from "./api"
import { Callout, label, machinePowerState, segment, time, useRemote } from "./common"
import { terminalHref } from "./routes"
import { ConnectDialog, WorkspaceActions } from "./workspace-actions"
import { Button, EmptyState, PageHeader, StatusBadge } from "./ui"
import { CreateMachineDialog } from "./features/machines/CreateMachineDialog"
import { DocumentationLink } from "./features/catalog/DocumentationLink"
import { DashboardIntro } from "./features/dashboard/DashboardIntro"
import styles from "./features/machines/machines.module.css"
import dashboard from "./features/dashboard/dashboard.module.css"
import type { Actions } from "./main"

export { CreateMachineDialog } from "./features/machines/CreateMachineDialog"

const memory = (mib: number) => mib >= 1024
  ? `${(mib / 1024).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB`
  : `${mib} MiB`
const regions = [
  { value: "all", label: "All regions" }, { value: "fra", label: "Frankfurt" }, { value: "nyc", label: "New York" },
]
const referenceRegion = (machine: Machine) => machine.machine_id === "fixture-cedar" ? "nyc" : "fra"

type Props = { rows?: Machine[]; error: unknown; actions: Actions; revision: number; defaultBoot?: BootMode; create?: boolean }
type Options = { value: string; label: string }[]
type Filter = "status" | "runtime" | "region" | "search" | "ownership"

function FilterMenu({ name, icon, items, value, open, onOpenChange, onChange }: {
  name: string; icon: ReactNode; items: Options; value: string; open: boolean
  onOpenChange: (open: boolean) => void; onChange: (value: string) => void
}) {
  const current = items.find(item => item.value === value)?.label
  return <Dropdown.Root open={open} onOpenChange={onOpenChange}>
    <Dropdown.Trigger className={styles.filterButton} title={name} aria-label={name} data-filtered={value !== "all"}>
      {icon}{open && <span>{current}</span>}
    </Dropdown.Trigger>
    <Dropdown.Portal><Dropdown.Content className={styles.filterMenu} align="end" sideOffset={5}>
      <Dropdown.RadioGroup value={value} onValueChange={onChange}>
        {items.map(item => <Dropdown.RadioItem className={styles.filterOption} key={item.value} value={item.value}>
          {item.label}<Dropdown.ItemIndicator><Check size={14} /></Dropdown.ItemIndicator>
        </Dropdown.RadioItem>)}
      </Dropdown.RadioGroup>
    </Dropdown.Content></Dropdown.Portal>
  </Dropdown.Root>
}

function CurrentUsage({ machine, revision }: { machine: Machine; revision: number }) {
  const running = machinePowerState(machine) === "running"
  const metric = useRemote<MachineMetrics>(running ? `machines/${segment(machine.machine_id)}/metrics` : "", revision)
  const sample = metric.data
  const availability = metricAvailability(sample, machine)
  const observedAt = sample?.observed_at && time(sample.observed_at)
  if (sample?.scope !== "machine" || !running || availability === "unavailable" || availability === "unknown") {
    return <span className={styles.usage} title={metric.error instanceof Error ? metric.error.message : undefined}>Metrics unavailable</span>
  }
  if (availability === "stale") return <span className={styles.usage} title={observedAt}>Metrics stale</span>
  const measurements = [[sample.memory_mb, "MEM"], [sample.disk_used_mb, "DSK"]] as const
  const formatted = measurements.flatMap(([value, title]) => {
    const measured = metricNumber(value)
    return measured === undefined ? [] : [`${title} ${measured} MB`]
  })
  return <span className={styles.usage} title={observedAt}>{formatted.join(" · ") || "Metrics unavailable"}</span>
}

function MachineCard({ machine, actions, revision }: { machine: Machine; actions: Actions; revision: number }) {
  const r = machine.resources ?? machine.spec?.resources
  const state = machinePowerState(machine)
  const region = fixtureScene ? referenceRegion(machine) : undefined
  const health = machine.health?.guest_management ?? machine.guest_management_status
  const runtime = machine.binding?.runtime_instance_id
  const href = "#/machines/" + segment(machine.machine_id)
  const lineage = fixtureScene && machine.machine_id === "fixture-fork" ? "Forked from atlas-dev" : undefined
  return <article className={styles.machineCard} data-power={state} data-fixture={Boolean(fixtureScene)}>
    <a href={href} className={styles.machineLink} title={machine.name}>
      <span className={styles.dot} aria-hidden="true" />
      <span className={styles.machineIdentity}>
        <strong>{machine.name}</strong>
        <small>{r ? [`${r.cpu_count} vCPU`, memory(r.memory_mib), r.storage_gib == null ? "" : `${r.storage_gib} GiB disk`].filter(Boolean).join(" · ") : "Resources unavailable"}</small>
      </span>
    </a>
    <div className={styles.machineTags}>
      {region && <span className={styles.tag}>{region}</span>}
      {fixtureScene && <span className={styles.tag}>Mine</span>}
      {!fixtureScene && machine.boot_mode && <span className={styles.tag}>{machine.boot_mode}</span>}
      <span className={styles.powerBadge}>{label(state)}</span>
    </div>
    <div className={styles.machineContext} title={machine.blocked_reasons?.join(", ")}>
      {lineage ? <><GitFork size={13} />{lineage}</> : fixtureScene ? <><Server size={13} />Standalone</> :
        <><Server size={13} />{health ? `Guest ${label(health).toLowerCase()}` : `Guest health unknown`}{runtime && <small>{runtime}</small>}</>}
    </div>
    <CurrentUsage machine={machine} revision={revision} />
    <div className={styles.machineActions}><WorkspaceActions machine={machine} actions={actions} compact surface="row" /></div>
  </article>
}

const creationScenarios = new Set(["03", "36", "43", "create-suggesting", "create-pending", "create-failed", "create-ready"])

function useCreate(create: boolean | undefined, showFixture = false) {
  const initial = fixtureScene && showFixture && creationScenarios.has(fixtureScene) ? fixtureScene : ""
  const [local, setLocal] = useState(Boolean(initial))
  return {
    open: local || create,
    show: () => setLocal(true),
    close: () => {
      setLocal(false)
      const [path, query] = location.hash.split("?")
      const params = new URLSearchParams(query)
      if (params.get("action") !== "new") return
      params.delete("action")
      location.hash = path + (params.size ? `?${params}` : "")
    },
  }
}

export function MachineList({ rows, error, actions, revision, defaultBoot, create }: Props) {
  const demo = Boolean(fixtureScene)
  const filteredRegions = ["02", "03", "04", "15", "19", "36", "41", "42", "43", "44"].includes(fixtureScene ?? "")
  const dialog = useCreate(create, true)
  const [search, setSearch] = useState("")
  const [status, setStatus] = useState("all")
  const [runtime, setRuntime] = useState("all")
  const [region, setRegion] = useState(filteredRegions ? "fra" : "all")
  const [ownership, setOwnership] = useState("all")
  const [filter, setFilter] = useState<Filter | null>(fixtureScene === "15" ? "region" : fixtureScene === "19" ? "status" : null)
  const [page, setPage] = useState(0)
  const data = rows ?? []
  const allStates = ["running", "standby", "hibernated", "stopped", "unknown"]
  const states = Array.from(new Set([...allStates, ...data.map(machinePowerState)]))
  const runtimes = Array.from(new Set(data.flatMap(machine => machine.binding?.runtime_instance_id ? [machine.binding.runtime_instance_id] : [])))
  const ownershipOptions = [["all", "All"], ["mine", "Mine"], ["organization", "Organization"], ["others", "Not mine"]].map(([value, label]) => ({ value, label }))
  const matching = data.filter(m =>
    (status === "all" || machinePowerState(m) === status) &&
    (runtime === "all" || m.binding?.runtime_instance_id === runtime) &&
    (region === "all" || referenceRegion(m) === region) &&
    (ownership === "all" || ownership === "mine") &&
    `${m.name} ${m.machine_id} ${m.template_id}`.toLowerCase().includes(search.trim().toLowerCase()),
  )
  const lastPage = Math.max(0, Math.ceil(matching.length / 10) - 1)
  const current = Math.min(lastPage, page)
  const selectFilter = (key: Filter) => (open: boolean) => setFilter(previous => open ? key : previous === key ? null : previous)
  const apply = (change: (value: string) => void) => (value: string) => { change(value); setPage(0) }
  const clear = () => { setSearch(""); setStatus("all"); setRuntime("all"); setRegion("all"); setOwnership("all"); setPage(0) }
  const otherRegion = data.filter(machine => referenceRegion(machine) !== region).length

  return <>
    <PageHeader title="Machines" className={styles.pageHeading} icon={<Server />} description="Live and stopped environments across your workspace."
      actions={<><DocumentationLink className={styles.docsLink} icon={false} /><Button variant="primary" onClick={dialog.show}><Plus size={15} /> New machine</Button></>} />
    <Callout error={error} />
    <div className={styles.toolbar}>
      <div className={styles.scopes} aria-label="Machine scope">
        {(demo ? ownershipOptions : [{ value: "all", label: "All machines" }]).map(item =>
          <button type="button" key={item.value} aria-pressed={ownership === item.value} onClick={() => apply(setOwnership)(item.value)}>{item.label}</button>)}
      </div>
      <div className={styles.filters}>
        {demo && <div className={styles.mobileScopes}><FilterMenu name="Machine scope" icon={<Users size={15} />} items={ownershipOptions}
          value={ownership} open={filter === "ownership"} onOpenChange={selectFilter("ownership")} onChange={apply(setOwnership)} /></div>}
        <FilterMenu name="Filter by status" icon={<Activity size={15} />} value={status} open={filter === "status"}
          onOpenChange={selectFilter("status")} onChange={apply(setStatus)}
          items={[{ value: "all", label: "All statuses" }, ...states.map(value => ({ value, label: label(value) }))]} />
        {runtimes.length > 1 && !demo && <FilterMenu name="Filter by runtime" icon={<Server size={15} />} value={runtime} open={filter === "runtime"}
          onOpenChange={selectFilter("runtime")} onChange={apply(setRuntime)}
          items={[{ value: "all", label: "All runtimes" }, ...runtimes.map(value => ({ value, label: value }))]} />}
        {demo && <FilterMenu name="Filter by region" icon={<MapPin size={15} />} items={regions}
          value={region} open={filter === "region"} onOpenChange={selectFilter("region")} onChange={apply(setRegion)} />}
        <button type="button" className={styles.filterButton} aria-label="Search machines" title="Search machines"
          aria-expanded={filter === "search"} data-filtered={Boolean(search)}
          onClick={() => selectFilter("search")(filter !== "search")}><Search size={15} /></button>
      </div>
    </div>
    {filter === "search" && <form className={styles.search} onSubmit={event => { event.preventDefault(); setFilter(null) }}>
      <Search size={15} /><input value={search} placeholder="Search by name, ID, or template"
        aria-label="Search by name, ID, or template" autoFocus onChange={event => apply(setSearch)(event.target.value)}
        onKeyDown={event => { if (event.key === "Escape") { event.stopPropagation(); setFilter(null) } }} />
      <button type="button" aria-label="Clear search" onClick={() => apply(setSearch)("")}><X size={14} /></button>
    </form>}
    {demo && region !== "all" && otherRegion > 0 && <p className={styles.contextNote}>{otherRegion} {otherRegion === 1 ? "machine" : "machines"} in {region === "fra" ? "New York" : "Frankfurt"}.{" "}
      <button type="button" onClick={() => apply(setRegion)("all")}>Show all regions</button></p>}
    <div className={styles.machineList}>
      {matching.slice(current * 10, current * 10 + 10).map(machine => <MachineCard key={machine.machine_id} machine={machine} actions={actions} revision={revision} />)}
      {!rows && !error && <div className={styles.skeleton} role="status" aria-label="Loading machines" />}
      {rows && !matching.length && <EmptyState icon={<Server />} title={data.length ? "No matching machines" : "No machines yet"}
        description={data.length ? "Change the filters to find machines." : "Create a machine from a local template to open your first workspace."}
        action={<Button variant="secondary" onClick={data.length ? clear : dialog.show}>{data.length ? "Clear filters" : "New machine"}</Button>} />}
    </div>
    {lastPage > 0 && <div className={styles.pagination}>
      <span>{matching.length} machines</span><span>
        <Button size="icon" variant="ghost" disabled={current === 0} aria-label="Previous page" onClick={() => setPage(current - 1)}><ChevronLeft size={16} /></Button>
        {current + 1} / {lastPage + 1}
        <Button size="icon" variant="ghost" disabled={current === lastPage} aria-label="Next page" onClick={() => setPage(current + 1)}><ChevronRight size={16} /></Button>
      </span>
    </div>}
    {dialog.open && <CreateMachineDialog actions={actions} revision={revision} defaultBoot={defaultBoot} close={dialog.close} />}
  </>
}

export function Dashboard({ rows, error, actions, revision, defaultBoot, create, onSearch, username }: Props & {
  onSearch?: () => void; username?: string
}) {
  const dialog = useCreate(create)
  const [connect, setConnect] = useState(false)
  const ops = useRemote<Page<Operation>>("operations?limit=4", revision)
  const metrics = useRemote<SystemMetrics>("system/metrics", revision)
  const machines = rows ?? []
  const running = machines.find(machine => machinePowerState(machine) === "running" &&
    (machine.health?.guest_management ?? machine.guest_management_status) === "ready")
  const allocation = machines.reduce((total, machine) => {
    const r = machine.resources ?? machine.spec?.resources
    if (!r) return total
    return {
      reported: total.reported + 1, disks: total.disks + Number(r.storage_gib != null),
      cpu: total.cpu + r.cpu_count, memory: total.memory + r.memory_mib, disk: total.disk + (r.storage_gib ?? 0),
    }
  }, { cpu: 0, memory: 0, disk: 0, reported: 0, disks: 0 })
  const now = fixtureScene ? new Date("2026-11-26T12:00:00Z") : new Date()
  const date = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "long", day: "numeric", ...(fixtureScene ? { timeZone: "UTC" } : {}) }).format(now)
  const observation = metrics.data
  const availability = metricAvailability(observation)
  const measured = (value: number | null | undefined, unit: string) => {
    const number = metricNumber(value)
    return number !== undefined && availability !== "unknown" && availability !== "unavailable"
      ? `${number.toLocaleString()} ${unit}` : "Unavailable"
  }
  return <div className={dashboard.page}>
    {fixtureScene === "01" && <DashboardIntro />}
    <section className={dashboard.greeting}>
      <time dateTime={now.toISOString()}>{date}</time>
      <h1>{username ? `Hello, ${username}` : "Welcome to your workspace"}</h1>
      {onSearch && <button type="button" className={dashboard.command} onClick={onSearch}><Search size={16} /><span>Search your workspace</span><kbd>Ctrl B</kbd></button>}
      <div className={dashboard.shortcuts}>
        <Button size="sm" variant="secondary" onClick={dialog.show}><Plus size={14} /> New machine</Button>
        {running && fixtureScene === "45" && <Button size="sm" onClick={() => setConnect(true)}><Smartphone size={14} /> SSH app · {running.name}</Button>}
        {running && <a href={terminalHref(running.machine_id)}><Terminal size={14} /> Web terminal · {running.name}</a>}
        <a href="#/machines"><Server size={14} /> All machines</a>
        <a href="#/snapshots"><Layers3 size={14} /> Snapshots</a>
        {!fixtureScene && <a href="#/operations"><Activity size={14} /> Operations</a>}
        <DocumentationLink />
      </div>
    </section>
    <section className={dashboard.fleet}>
      <header><h2>Your machines</h2>{rows && <span>{Math.min(4, machines.length)}/{machines.length}</span>}</header>
      <Callout error={error} />
      {!rows && !error && <div className={styles.skeleton} role="status" aria-label="Loading machines" />}
      {rows && !rows.length && <EmptyState icon={<Server />} title="No machines yet" action={<Button onClick={dialog.show}><Plus size={14} /> New machine</Button>} />}
      <div className={styles.machineList}>{machines.slice(0, 4).map(machine =>
        <MachineCard key={machine.machine_id} machine={machine} actions={actions} revision={revision} />)}</div>
    </section>
    {!fixtureScene && <div className={dashboard.resources}>
      <section>
        <header><h2>Recent operations</h2><a href="#/operations">View all <ArrowUpRight size={13} /></a></header>
        <Callout error={ops.error} />
        {ops.data?.items.map(item => <a className={dashboard.operation} key={item.operation_id} href={`#/operations/${segment(item.operation_id)}`}>
          <span>{label(item.kind)}<small>{machines.find(machine => machine.machine_id === item.machine_id)?.name ?? item.machine_id ?? item.operation_id}</small></span>
          <StatusBadge value={item.state} />
        </a>)}
        {ops.data?.items.length === 0 && <p>No operations yet.</p>}
      </section>
      <section>
        <header><h2>Configured resources</h2></header>
        {[
          { Icon: Cpu, title: "Virtual CPUs", value: rows && (!machines.length || allocation.reported) ? `${allocation.cpu} vCPU` : "Unavailable" },
          { Icon: MemoryStick, title: "Memory", value: rows && (!machines.length || allocation.reported) ? memory(allocation.memory) : "Unavailable" },
          { Icon: HardDrive, title: "Guest storage capacity", value: rows && (!machines.length || allocation.disks) ? `${allocation.disk} GiB` : "Unavailable" },
        ].map(({ Icon, title, value }) => <div className={dashboard.resource} key={title}><Icon size={16} /><span>{title}</span><strong>{value}</strong></div>)}
        {allocation.reported < machines.length && <p>Resources reported by {allocation.reported} of {machines.length} machines.</p>}
        {allocation.disks < allocation.reported && <p>Guest storage capacity reported by {allocation.disks} of {machines.length} machines.</p>}
      </section>
      <section>
        <header><h2>Runtime usage</h2>{observation && <StatusBadge value={availability} />}</header>
        <Callout error={metrics.error} />
        {observation ? <>
          <p>{observation.scope ? label(observation.scope) : "Scope unavailable"}{observation.source && ` · ${label(observation.source)}`}{observation.observed_at && ` · ${time(observation.observed_at)}`}</p>
          <div className={dashboard.resource}><Cpu size={16} /><span>CPU in use</span><strong>{measured(observation.used_cpus, "vCPU")}</strong></div>
          <div className={dashboard.resource}><MemoryStick size={16} /><span>Memory in use</span><strong>{measured(observation.used_memory_mb, "MB")}</strong></div>
          <div className={dashboard.resource}><HardDrive size={16} /><span>Disk in use</span><strong>{measured(observation.used_disk_gb, "GB")}</strong></div>
          {Object.values(observation.issues ?? {}).map(issue => <p key={issue.code + issue.message}>{issue.message}</p>)}
        </> : !metrics.error && <p role="status">Loading runtime metrics…</p>}
      </section>
    </div>}
    {dialog.open && <CreateMachineDialog actions={actions} revision={revision} defaultBoot={defaultBoot} close={dialog.close} />}
    {running && connect && <ConnectDialog machine={running} actions={actions} open={connect} onOpenChange={setConnect} />}
  </div>
}
