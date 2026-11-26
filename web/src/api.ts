import { Client } from "../../sdk/typescript/src/index"
import type { Machine, MetricObservation, Service, ServiceObservation } from "../../sdk/typescript/src/index"
import { useEffect, useState } from "react"

const fixture = import.meta.env.DEV ? new URLSearchParams(location.search).get("fixture") : null
const local = import.meta.env.DEV && fixture ? await import("./fixtures/scenes") : undefined

export const fixtureScene = fixture && local?.fixtureRoutes.has(fixture) ? fixture : undefined
export const fixtureHash = fixtureScene ? local?.fixtureRoutes.get(fixtureScene) : undefined
export const api: Client = import.meta.env.DEV && fixtureScene
  ? new (await import("./fixtures/client")).FixtureClient(fixtureScene)
  : new Client(location.origin)
export const segment = encodeURIComponent
export async function collection<T>(path: string, signal: AbortSignal): Promise<T[]> {
  const result: T[] = []
  for await (const item of api.pages<T>(path, 100, signal)) result.push(item)
  return result
}

export function useCollection<T>(path: string, revision: number): { items?: T[]; error?: unknown } {
  const [result, setResult] = useState<{ path: string; items?: T[]; error?: unknown }>()
  useEffect(() => {
    if (!path) return
    const abort = new AbortController()
    collection<T>(path, abort.signal)
      .then((items) => {
        if (!abort.signal.aborted) setResult({ path, items })
      })
      .catch((error) => {
        if (!abort.signal.aborted) setResult({ path, error })
      })
    return () => abort.abort()
  }, [path, revision])
  return result?.path === path ? result : {}
}

export function metricAvailability(sample?: MetricObservation, machine?: Machine): NonNullable<MetricObservation["availability"]> | "unknown" {
  const availability = sample?.availability
  const execution = sample?.execution
  if (!availability) return "unknown"
  if ((availability === "available" || availability === "partial") && machine && execution) {
    const binding = machine.binding
    if (execution.binding_generation !== machine.binding_generation ||
        (binding?.binding_id && execution.binding_id !== binding.binding_id) ||
        (binding?.incarnation_id && execution.incarnation_id !== binding.incarnation_id) ||
        (binding?.execution_epoch !== undefined && execution.execution_epoch !== binding.execution_epoch)) {
      return "stale"
    }
  }
  return availability
}

export function metricNumber(value: number | null | undefined): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined
}

export function currentServiceObservation(service: Service, machine?: Machine): ServiceObservation | undefined {
  const observation = service.observation
  if (!observation) return
  const oldRevision = observation.service_revision !== service.revision
  const scoped = observation.scope !== "configuration"
  const power = machine?.power_state ?? machine?.observed_state
  const matching = machine?.machine_id === service.machine_id && power === "running" &&
    observation.binding_generation === machine.binding_generation &&
    observation.execution_epoch !== undefined && observation.execution_epoch === machine.binding?.execution_epoch
  return oldRevision || (scoped && !matching && observation.status !== "unknown")
    ? { ...observation, status: "stale" }
    : observation
}
