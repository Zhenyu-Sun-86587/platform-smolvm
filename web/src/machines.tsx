import { useState } from "react"
import {
  Activity,
  ArrowUpRight,
  Box,
  ChevronLeft,
  ChevronRight,
  Clock3,
  Cpu,
  HardDrive,
  MemoryStick,
  Plus,
  Search,
  Server,
} from "lucide-react"
import type { BootMode, CreateMachine, Machine, Operation, Page, RuntimeProfile } from "../../sdk/typescript/src/index"
import { useCollection } from "./api"
import { Callout, label, machinePowerState, num, segment, State, str, time, useRemote } from "./common"
import { WorkspaceActions } from "./workspace-actions"
import { PageIntro } from "./daytona/page"
import { Button } from "./daytona/button"
import { Empty, EmptyHeader, EmptyMedia, EmptyTitle, EmptyDescription } from "./daytona/empty"
import { Table, TableHeader, TableBody, TableRow } from "./daytona/table"
import { Skeleton } from "./daytona/skeleton"
import type { Actions } from "./main"

const memory = (mib: number) =>
  mib >= 1024 ? (mib / 1024).toLocaleString(undefined, { maximumFractionDigits: 2 }) + " GiB" : mib + " MiB"
type Template = { template_id?: string; id?: string; name: string; version: number; spec?: {boot_mode?: BootMode} }

export function MachineList({
  rows,
  error,
  actions,
  revision,
  defaultBoot = "oci",
}: {
  rows?: Machine[]
  error: unknown
  actions: Actions
  revision: number
  defaultBoot?: BootMode
}) {
  const templates = useCollection<Template>("templates", revision)
  const hasSystemTemplates = templates.items?.some(template => template.spec?.boot_mode === "system") ?? false
  const profiles = useCollection<RuntimeProfile>(hasSystemTemplates ? "runtime-profiles" : "", revision)
  const systemRuntimes = profiles.items?.filter(profile => profile.capabilities["boot.system"]?.status === "supported") ?? []
  const choices = (templates.items ?? []).flatMap(template => {
    const value = template.template_id ?? template.id
    if (!value) return []
    const system = template.spec?.boot_mode === "system"
    return [{value, label: system ? `${template.name} · System` : template.name, system,
      version: template.version, disabled: system && systemRuntimes.length === 0}]
  })
  const available = choices.filter(item => !item.disabled)
  const matchingBoot = available.filter(item => item.system === (defaultBoot === "system"))
  const preferred = (matchingBoot.length ? matchingBoot : available).reduce<typeof choices[number] | undefined>(
    (last, item) => (!last || item.version > last.version ? item : last),
    undefined,
  )
  const [search, setSearch] = useState(""),
    [filter, setFilter] = useState("all"),
    [page, setPage] = useState(0)
  const ops = useRemote<Page<Operation>>("operations?limit=4", revision)
  const data = rows ?? []
  const states = Array.from(
    new Set(["all", "running", "standby", "hibernated", "stopped", ...data.map(machinePowerState)]),
  )
  const matching = data.filter(
    (m) =>
      (filter === "all" || machinePowerState(m) === filter) &&
      (m.name + " " + m.machine_id).toLowerCase().includes(search.trim().toLowerCase()),
  )
  const lastPage = Math.max(0, Math.ceil(matching.length / 10) - 1),
    current = Math.min(lastPage, page)
  const withResources=data.filter(m=>m.resources||m.spec?.resources)
  const allocation = withResources.reduce(
    (totals, m) => {
      const r = m.resources ?? m.spec?.resources
      return {
        cpu: totals.cpu + (r?.cpu_count ?? 0),
        mem: totals.mem + (r?.memory_mib ?? 0),
        disk: totals.disk + (r?.storage_gib ?? 0),
      }
    },
    { cpu: 0, mem: 0, disk: 0 },
  )
  const create = () =>
    actions.dialog({
      title: "Create machine",
      submit: "Create machine",
      sheet: true,
      description: "Choose a template and allocate resources for your guest.",
      fields: (values) => {
        const selected = choices.find(item => item.value === (values?.get("template_id") ?? preferred?.value))
        const common = [
          {name: "name", label: "Machine name", required: true},
          {name: "template_id", label: "Template", value: preferred?.value, required: true, options: choices,
            note: hasSystemTemplates && systemRuntimes.length === 0 ? "System templates require a verified runtime." : undefined},
          {name: "cpu", label: "vCPUs", type: "number" as const, min: 1, value: 2, required: true},
          {name: "memory", label: "Memory (MiB)", type: "number" as const, min: 1, value: 2048, required: true},
        ]
        if (selected?.system) {
          return [...common, {name: "runtime_instance_id", label: "Runtime", required: true,
            options: systemRuntimes.map(runtime => ({value: runtime.runtime_instance_id}))}]
        }
        const idle = [
          {value: "default", label: "Use template default"},
          {value: "enabled", label: "Hibernate when idle"},
          {value: "disabled", label: "Keep running"},
        ]
        return [...common,
          {name: "storage", label: "Storage (GiB)", type: "number", min: 1, value: 20, required: true},
          {name: "overlay", label: "Overlay (GiB)", type: "number", min: 1, value: 10, required: true},
          {name: "auto_idle", label: "Idle policy", options: idle, value: "default"},
          ...(values?.get("auto_idle") === "enabled" ? [
            {name: "idle", label: "Idle timeout (seconds)", type: "number" as const, min: 60, value: 1800, required: true},
          ] : []),
          {name: "start", label: "Start after creation", type: "checkbox", value: true},
        ]
      },
      act: (form) => {
        const selected = choices.find(item => item.value === str(form, "template_id"))
        const resources = {cpu_count: num(form, "cpu"), memory_mib: num(form, "memory")}
        const body: CreateMachine = {
          name: str(form, "name"),
          template_id: str(form, "template_id"),
          resources,
          start: selected?.system || form.has("start"),
        }
        if (selected?.system) {
          body.boot_mode = "system"
          body.runtime_instance_id = str(form, "runtime_instance_id")
        } else {
          body.boot_mode = "oci"
          body.resources = {...resources, storage_gib: num(form, "storage"), overlay_gib: num(form, "overlay")}
          const idle = str(form, "auto_idle")
          if (idle !== "default") body.idle_policy = {enabled: idle === "enabled", timeout_seconds: idle === "enabled" ? num(form, "idle") : 1800}
        }
        return actions.mutate("machines", body)
      },
    })
  return (
    <>
      <PageIntro
        title="Machines"
        desc="Create, connect to, and branch your workspaces."
        actions={
          <Button onClick={create} disabled={!preferred} title={!preferred && hasSystemTemplates ? "System templates require a verified runtime." : undefined}>
            <Plus />
            New machine
          </Button>
        }
      />
      <Callout error={error} />
      <Callout error={templates.error} />
      <Callout error={profiles.error} />
      <div className="toolbar">
        <div className="segments" aria-label="Filter by machine state">
          {states.map((state) => (
            <button
              key={state}
              aria-pressed={state === filter}
              onClick={() => {
                setFilter(state)
                setPage(0)
              }}
            >
              {state === "all" ? "All machines" : label(state)}
              {(state === "all" || state === filter) && rows && (
                <b>{state === "all" ? rows.length : rows.filter((m) => machinePowerState(m) === state).length}</b>
              )}
            </button>
          ))}
        </div>
        <label className="search-box">
          <Search size={15} />
          <input
            aria-label="Filter machines"
            placeholder="Search machines…"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value)
              setPage(0)
            }}
          />
        </label>
      </div>
      <div className="table-frame">
        <div className="table-wrap">
          <Table>
            <TableHeader>
              <TableRow>
                {["Machine", "State", "Resources", "Last activity", ""].map((h, i) => (
                  <th key={i} scope="col">
                    {h || <span className="sr-only">Actions</span>}
                  </th>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {matching.slice(current * 10, current * 10 + 10).map((m) => {
                const r = m.resources ?? m.spec?.resources
                return (
                  <TableRow key={m.machine_id}>
                    <td>
                      <div className="machine-cell">
                        <span className="machine-icon">
                          <Box size={18} />
                        </span>
                        <div>
                          <a href={"#/machines/" + segment(m.machine_id)}>{m.name}</a>
                          <small title={m.machine_id}>{m.machine_id.slice(0, 12)}</small>
                        </div>
                      </div>
                    </td>
                    <td>
                      <State value={machinePowerState(m)} />
                      <small title={m.blocked_reasons?.join(", ")}>
                        {m.health ? `Guest ${label(m.health.guest_management)} · Workload ${label(m.health.workload)}` : label(m.availability ?? "unknown")}
                      </small>
                    </td>
                    <td>
                      {r ? (
                        <>
                          {r.cpu_count} vCPU · {memory(r.memory_mib)}
                          {r.storage_gib !== undefined && <small>{r.storage_gib} GiB disk</small>}
                        </>
                      ) : (
                        "—"
                      )}
                    </td>
                    <td className="subtle">{time(m.last_user_activity_at)}</td>
                    <td>
                      <WorkspaceActions machine={m} actions={actions} compact />
                    </td>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
        {!rows && !error && (
          <div className="loading">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        )}
        {rows && !matching.length && (
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <Server />
              </EmptyMedia>
              <EmptyTitle>{data.length ? "No matching machines" : "No machines yet"}</EmptyTitle>
              <EmptyDescription>
                {data.length
                  ? "Try another state or search term."
                  : "Create a machine from a local template to open your first workspace."}
              </EmptyDescription>
            </EmptyHeader>
            <Button
              size="sm"
              variant="outline"
              disabled={!data.length && !preferred}
              onClick={() => {
                if (data.length) {
                  setFilter("all")
                  setSearch("")
                } else create()
              }}
            >
              {data.length ? "Clear filters" : "Create machine"}
            </Button>
          </Empty>
        )}
        <div className="table-footer">
          <span>
            {matching.length} {matching.length === 1 ? "machine" : "machines"}
          </span>
          <div>
            <Button
              variant="ghost"
              size="icon-xs"
              aria-label="Previous page"
              disabled={!current}
              onClick={() => setPage(current - 1)}
            >
              <ChevronLeft />
            </Button>
            <span>
              {current + 1} / {lastPage + 1}
            </span>
            <Button
              variant="ghost"
              size="icon-xs"
              aria-label="Next page"
              disabled={current === lastPage}
              onClick={() => setPage(current + 1)}
            >
              <ChevronRight />
            </Button>
          </div>
        </div>
      </div>
      <div className="workspace-summary">
        <section>
          <header>
            <h2>
              <Activity />
              Recent operations
            </h2>
            <a href="#/operations">
              View all <ArrowUpRight size={13} />
            </a>
          </header>
          <Callout error={ops.error} />
          {ops.data?.items.map((o) => (
            <a
              className="recent-operation"
              key={o.operation_id}
              href={"#/operations/" + segment(o.operation_id)}
            >
              <span className="operation-icon">
                <Clock3 size={14} />
              </span>
              <div>
                {label(o.kind)}
                <small>
                  {data.find((m) => m.machine_id === o.machine_id)?.name ?? o.machine_id ?? o.operation_id}
                </small>
              </div>
              <State value={o.state} />
            </a>
          ))}
          {ops.data && !ops.data.items.length && <p className="empty">No operations yet.</p>}
        </section>
        <section>
          <header>
            <h2>
              <Cpu />
              Allocated resources
            </h2>
          </header>
          {[
            {Icon:Cpu, title:"Virtual CPUs", value:allocation.cpu + " vCPU"},
            {Icon:MemoryStick, title:"Memory", value:memory(allocation.mem)},
            {Icon:HardDrive, title:"Guest storage", value:allocation.disk + " GiB"},
          ].map(({Icon,title,value}) => {
            return (
              <div className="resource-row" key={title}>
                <Icon size={15} />
                <span>{title}</span>
                <strong>{rows && (!rows.length || withResources.length) ? value : "—"}</strong>
              </div>
            )
          })}
          {withResources.length<data.length&&<p className="resource-coverage">Resources reported by {withResources.length} of {data.length} machines.</p>}
        </section>
      </div>
    </>
  )
}
