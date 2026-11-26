import { useEffect, useState } from "react"
import type { Machine, MachineMetrics } from "../../../../sdk/typescript/src/index"
import { machine as machinePath } from "../../../../sdk/typescript/src/index"
import { bundleBound, Callout, label, time, useRemote } from "../../common"
import { fixtureScene, metricAvailability, metricNumber } from "../../api"
import styles from "./detail.module.css"

const units = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 })
const observationInterval = 15000
const maxGap = observationInterval * 4
const maxSamples = 24
const definitions = [
  { name: "CPU", field: "cpu_millis", unit: "ms", description: "Cumulative CPU time" },
  { name: "Memory", field: "memory_mb", unit: "MB", description: "Runtime memory measurement" },
  { name: "Disk", field: "disk_used_mb", unit: "MB", description: "Runtime disk measurement" },
] as const
type MetricField = typeof definitions[number]["field"]
type Sample = { at: number; values: Partial<Record<MetricField, number>> }

function measure(raw: number | undefined, unit: string) {
  return raw === undefined ? undefined : units.format(raw) + " " + unit
}

function sampleOf(data: MachineMetrics | undefined) {
  if (!data) return undefined
  const values: Sample["values"] = {}
  for (const { field } of definitions) {
    const value = metricNumber(data[field])
    if (value !== undefined && !data.issues?.[field]) values[field] = value
  }
  return { at: Date.parse(data.observed_at ?? data.at ?? ""), values }
}

function plotSamples(source: Sample[] | undefined, sample: Sample) {
  const last = source?.at(-1)
  if (!last || sample.at <= last.at || sample.at - last.at > maxGap) return [sample]
  return [...(source ?? []), sample].slice(-maxSamples)
}

const referenceSamples: Sample[] = [
  { cpu_millis: 15, memory_mb: 422, disk_used_mb: 1760 },
  { cpu_millis: 32, memory_mb: 421, disk_used_mb: 1760 },
  { cpu_millis: 50, memory_mb: 578, disk_used_mb: 1770 },
  { cpu_millis: 110, memory_mb: 581, disk_used_mb: 1790 },
  { cpu_millis: 130, memory_mb: 612, disk_used_mb: 1830 },
  { cpu_millis: 196, memory_mb: 706, disk_used_mb: 1860 },
  { cpu_millis: 240, memory_mb: 746, disk_used_mb: 1880 },
].map((values, index) => ({
  at: Date.UTC(2026, 10, 26, 0, 0, index * 15),
  values,
}))

export function ResourceUsage({ machine, revision }: { machine: Machine; revision: number }) {
  const [sampleRevision, setSampleRevision] = useState(0)
  const [history, setHistory] = useState<{ key: string; samples: Sample[] }>()
  const { data, error } = useRemote<MachineMetrics>(machinePath(machine.machine_id) + "/metrics", revision + sampleRevision)
  const status = data && bundleBound(machine) && !data.execution && !fixtureScene && (data.availability === "available" || data.availability === "partial")
    ? "unknown" : metricAvailability(data, machine)
  const current = status === "available" || status === "partial"
  const sample = current ? sampleOf(data) : undefined
  const identity = JSON.stringify([machine.machine_id, machine.binding?.binding_id, machine.binding_generation,
    machine.binding?.incarnation_id, machine.binding?.execution_epoch, data?.source, data?.scope])
  const seriesFixture = current && (fixtureScene === "37" || fixtureScene === "44")
  const samples = seriesFixture ? referenceSamples
    : current && sample && !Number.isNaN(sample.at) ? history?.key === identity ? history.samples : [sample] : []

  useEffect(() => {
    if (fixtureScene) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") setSampleRevision(value => value + 1)
    }, observationInterval)
    return () => clearInterval(timer)
  }, [])

  useEffect(() => {
    if (seriesFixture) return
    if (!current) {
      if (data || error) setHistory(undefined)
      return
    }
    if (!sample || Number.isNaN(sample.at)) return
    setHistory(previous => {
      const prior = previous?.key === identity ? previous.samples : undefined
      const last = prior?.at(-1)
      if (last?.at === sample.at && JSON.stringify(last.values) === JSON.stringify(sample.values)) return previous
      return { key: identity, samples: plotSamples(prior, sample) }
    })
  }, [current, data, error, identity, seriesFixture])

  const timestamp = data?.observed_at ?? data?.at
  const source = data?.source ? label(data.source) : undefined
  const scope = data?.scope ? label(data.scope) : undefined
  return <section aria-labelledby="machine-usage-title">
    <div className={styles.metricsHead}>
      <h2 className={styles.sectionTitle} id="machine-usage-title">Resource usage</h2>
      <span>{data ? [label(status), source, scope].filter(Boolean).join(" · ") : error ? "Unavailable" : "Loading…"}</span>
    </div>
    <Callout error={error} />
    <div className={styles.metrics}>{definitions.map(({ name, field, unit, description }) => <div className={styles.metric} key={field}>
      <h3>{name}</h3>
      <strong>{measure(sample?.values[field], unit) ?? (status === "stale" ? "Stale" : "Unavailable")}</strong>
      <span>{description}</span>
      <SamplePlot samples={samples} field={field} name={name} unit={unit} />
    </div>)}</div>
    {(timestamp || samples.length > 1) && <p className={styles.note}>
      {timestamp && <>Observed <time dateTime={timestamp}>{time(timestamp)}</time></>}
      {samples.length > 1 && <> · {samples.length} browser-session samples</>}
    </p>}
    {Object.entries(data?.issues ?? {}).map(([key, issue]) => <p className={styles.note} key={key}>{issue.message}</p>)}
  </section>
}

function SamplePlot({ samples, field, name, unit }: { samples: Sample[]; field: MetricField; name: string; unit: string }) {
  const points = samples.flatMap(sample => {
    const value = sample.values[field]
    return value === undefined ? [] : [value]
  })
  const max = Math.max(...points, 1)
  const first = samples[0]?.at ?? 0
  const duration = (samples.at(-1)?.at ?? first) - first
  const coords = samples.map(({ at, values }) => {
    const value = values[field]
    if (value === undefined) return undefined
    const x = duration ? 4 + 112 * (at - first) / duration : 60
    const y = 26 - 21 * value / max
    return { x, y, value, at }
  })
  return <div className={styles.metricGrid}>
    {points.length > 0 && <svg viewBox="0 0 120 30" preserveAspectRatio="none" role="img"
      aria-label={`${points.length} ${name} ${points.length === 1 ? "observation" : "observations"} in ${unit}`}>
      {coords.map((point, index) => {
        if (!point) return null
        const previous = coords[index - 1]
        const bridges = previous && (field !== "cpu_millis" || point.value >= previous.value)
        return <g key={point.at}>
          {bridges && <line x1={previous.x} y1={previous.y} x2={point.x} y2={point.y} stroke="currentColor" strokeWidth="1.4" />}
          <circle cx={point.x} cy={point.y} r={points.length === 1 ? 2 : 1.3} fill="currentColor">
            <title>{measure(point.value, unit)} · {time(new Date(point.at).toISOString())}</title>
          </circle>
        </g>
      })}
    </svg>}
  </div>
}
