import { useEffect, useState } from "react"
import { Box, GitBranch, GitFork, Layers2 } from "lucide-react"
import type { Machine } from "../../sdk/typescript/src/index"
import { api, segment, useCollection } from "./api"
import { Callout, label, Section, State } from "./common"
import { Button } from "./daytona/button"
import { copyDialog } from "./workspace-actions"
import type { Actions } from "./main"

type Edge = {
  parent_machine_id: string
  child_machine_id: string
  kind: string
  status: string
  observed_at?: string
}
type Graph = { edges: Edge[]; errors: unknown[] }
async function readFamily(id: string, signal: AbortSignal): Promise<Graph> {
  const queue = [id],
    seen = new Set(queue),
    edges = new Map<string, Edge>(),
    errors: unknown[] = []
  for (let i = 0; i < queue.length; i++) {
    try {
      const page = await api.get<{ items: Edge[] }>("machines/" + segment(queue[i]) + "/dependencies", signal)
      for (const edge of page.items) {
        edges.set(JSON.stringify(edge), edge)
        for (const neighbor of [edge.parent_machine_id, edge.child_machine_id]) {
          if (!seen.has(neighbor)) {
            seen.add(neighbor)
            queue.push(neighbor)
          }
        }
      }
    } catch (error) {
      if (signal.aborted) throw error
      errors.push(error)
    }
  }
  return { edges: [...edges.values()], errors }
}
export function Branches({
  machine,
  actions,
  revision,
}: {
  machine: Machine
  actions: Actions
  revision: number
}) {
  const [graph, setGraph] = useState<Graph>()
  const names = useCollection<Machine>("machines", revision)
  useEffect(() => {
    const abort = new AbortController()
    readFamily(machine.machine_id, abort.signal)
      .then((result) => {
        if (!abort.signal.aborted) setGraph(result)
      })
      .catch((error) => {
        if (!abort.signal.aborted) setGraph({ edges: [], errors: [error] })
      })
    return () => abort.abort()
  }, [machine.machine_id, revision])
  const lookup = new Map(names.items?.map((m) => [m.machine_id, m]))
  lookup.set(machine.machine_id, machine)
  const outgoing = new Map<string, Map<string, Edge[]>>(),
    children = new Set<string>()
  for (const e of graph?.edges ?? []) {
    const family = outgoing.get(e.parent_machine_id) ?? new Map<string, Edge[]>()
    family.set(e.child_machine_id, [...(family.get(e.child_machine_id) ?? []), e])
    outgoing.set(e.parent_machine_id, family)
    children.add(e.child_machine_id)
  }
  const roots = [...outgoing.keys()].filter((id) => !children.has(id))
  if (!roots.length) roots.push(machine.machine_id)
  function node(id: string, edges: Edge[], ancestors: Set<string>) {
    const m = lookup.get(id),
      current = id === machine.machine_id,
      cycle = ancestors.has(id)
    const next = new Set(ancestors).add(id),
      family = outgoing.get(id)
    return (
      <li key={id}>
        <div className={"branch-node" + (current ? " current" : "")}>
          <span className="machine-icon">{edges.length ? <GitBranch size={17} /> : <Box size={17} />}</span>
          <div>
            <a href={"#/machines/" + segment(id) + "/branches"}>{m?.name ?? id}</a>
            {current && <em>Current</em>}
            <small>{id}</small>
          </div>
          {m && <State value={m.observed_state} />}
        </div>
        {edges.length > 0 && (
          <div className="edge-info">
            {edges.map((e, i) => (
              <span key={i}>
                {e.kind === "logical_origin"
                  ? "Logical origin"
                  : e.kind === "live_branch_resource"
                    ? "Native storage dependency"
                    : label(e.kind)}{" "}
                · {e.status}
              </span>
            ))}
          </div>
        )}
        {!cycle && family && <ul>{[...family].map(([child, e]) => node(child, e, next))}</ul>}
      </li>
    )
  }
  return (
    <Section
      title="Branch tree"
      actions={
        <>
          <Button
            variant="outline"
            size="sm"
            disabled={machine.capabilities?.independent_clone?.supported === false}
            onClick={() => copyDialog(machine, actions, false)}
          >
            <Layers2 />
            Independent Clone
          </Button>
          <Button
            size="sm"
            disabled={
              machine.observed_state !== "running" || machine.capabilities?.live_fork?.supported === false
            }
            onClick={() => copyDialog(machine, actions, true)}
          >
            <GitFork />
            Live Fork
          </Button>
        </>
      }
    >
      <Callout error={names.error} />
      {graph?.errors.map((e, i) => <Callout key={i} error={e} />)}
      {graph ? (
        <div className="tree-canvas">
          <ul className="branch-tree">{roots.map((id) => node(id, [], new Set()))}</ul>
        </div>
      ) : (
        <p className="empty">Loading branch tree…</p>
      )}
      <div className="tree-note">
        <GitBranch size={15} />
        Logical origin records ancestry. An active native storage dependency keeps the parent’s backing
        storage in use.
      </div>
    </Section>
  )
}
