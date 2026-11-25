import { useEffect, useState } from "react"
import type { Machine, Service, Task } from "../../sdk/typescript/src/index"
import { ArrowUpRight, Globe2, ListChecks } from "lucide-react"
import { collection, segment } from "./api"
import { Callout, label, State, time } from "./common"
import { PageIntro } from "./daytona/page"
import { Table, TableHeader, TableBody, TableRow } from "./daytona/table"
import { Empty, EmptyHeader, EmptyMedia, EmptyTitle, EmptyDescription } from "./daytona/empty"
import { Skeleton } from "./daytona/skeleton"
import { Services, Snapshots, Tasks } from "./machine"
import type { Actions } from "./main"

type PageName = "snapshots" | "tasks" | "networking"
const descriptions = {
  snapshots: "Capture full machine state, restore a workspace, or move an archive between hosts.",
  tasks: "Follow persistent guest processes and inspect their output.",
  networking: "Manage published guest ports, domains, and connection details.",
}
type Row = { id: string; name: string; machine: string; state?: string; detail: string; href: string }
async function readRows(page: PageName, id: string, signal: AbortSignal): Promise<Row[]> {
  const path = "machines/" + segment(id)
  if (page === "tasks")
    return (await collection<Task>(path + "/tasks", signal)).map((t) => {
      const key = t.task_id ?? t.id ?? ""
      return {
        id: key,
        name: key,
        machine: id,
        state: t.state,
        detail: time(t.observed_at),
        href: "#/" + path + "/tasks/" + segment(key),
      }
    })
  return (await collection<Service>(path + "/services", signal)).map((s) => ({
    id: s.service_id,
    name: s.name,
    machine: id,
    state: s.enabled === false ? "disabled" : "enabled",
    detail: (s.public_host ?? s.protocol) + " → :" + s.guest_port,
    href: "#/" + path + "/networking",
  }))
}
export function Catalog({
  page,
  machines,
  actions,
  revision,
}: {
  page: PageName
  machines?: Machine[]
  actions: Actions
  revision: number
}) {
  const [selected, setSelected] = useState("")
  const [list, setList] = useState<{ rows: Row[]; errors: unknown[] }>()
  const machine = machines?.find((m) => m.machine_id === selected)
  const keys = machines ? JSON.stringify(machines.map((m) => m.machine_id)) : undefined
  useEffect(() => {
    if (!keys || page === "snapshots" || selected) return
    const ids: string[] = JSON.parse(keys),
      abort = new AbortController()
    async function load() {
      const rows: Row[] = [],
        errors: unknown[] = []
      for (let i = 0; i < ids.length && !abort.signal.aborted; i += 5) {
        const batch = await Promise.allSettled(
          ids.slice(i, i + 5).map((id) => readRows(page, id, abort.signal)),
        )
        for (const r of batch) {
          if (r.status === "fulfilled") rows.push(...r.value)
          else errors.push(r.reason)
        }
      }
      if (!abort.signal.aborted) setList({ rows, errors })
    }
    void load()
    return () => abort.abort()
  }, [keys, selected, page, revision])
  return (
    <>
      <PageIntro
        title={label(page)}
        desc={descriptions[page]}
        actions={
          <select
            aria-label="Machine scope"
            value={machine?.machine_id ?? ""}
            onChange={(e) => setSelected(e.target.value)}
          >
            <option value="">All machines</option>
            {machines?.map((m) => (
              <option key={m.machine_id} value={m.machine_id}>
                {m.name}
              </option>
            ))}
          </select>
        }
      />
      {page === "snapshots" ? (
        <Snapshots machine={machine} machines={machines} actions={actions} revision={revision} />
      ) : machine ? (
        page === "tasks" ? (
          <Tasks key={machine.machine_id} machine={machine} actions={actions} revision={revision} />
        ) : (
          <Services key={machine.machine_id} machine={machine} actions={actions} revision={revision} />
        )
      ) : (
        <>
          {list?.errors.map((e, i) => <Callout key={i} error={e} />)}
          {!list ? (
            <Skeleton className="h-24" />
          ) : (
            <div className="table-frame">
              <div className="table-wrap">
                <Table>
                  <TableHeader>
                    <TableRow>
                      {[
                        page === "tasks" ? "Task" : "Service",
                        "Machine",
                        "State",
                        page === "tasks" ? "Last observation" : "Endpoint",
                        "",
                      ].map((v, i) => (
                        <th key={i}>{v}</th>
                      ))}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {list.rows.map((r) => (
                      <TableRow key={r.id}>
                        <td>
                          <a href={r.href}>{r.name}</a>
                        </td>
                        <td>{machines?.find((m) => m.machine_id === r.machine)?.name ?? r.machine}</td>
                        <td>
                          <State value={r.state} />
                        </td>
                        <td>{r.detail}</td>
                        <td>
                          <a className="inline-link" href={r.href}>
                            {page === "tasks" ? "View logs" : "Manage"}
                            <ArrowUpRight size={13} />
                          </a>
                        </td>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              {!list.rows.length && !list.errors.length && (
                <Empty>
                  <EmptyHeader>
                    <EmptyMedia variant="icon">{page === "tasks" ? <ListChecks /> : <Globe2 />}</EmptyMedia>
                    <EmptyTitle>
                      {page === "tasks" ? "No persistent tasks" : "No published services"}
                    </EmptyTitle>
                    <EmptyDescription>
                      {machines?.length
                        ? "Choose a machine above to " +
                          (page === "tasks" ? "start a task." : "publish a guest port.")
                        : "Create a machine to get started."}
                    </EmptyDescription>
                  </EmptyHeader>
                </Empty>
              )}
            </div>
          )}
        </>
      )}
    </>
  )
}
