/// <reference types="vite/client" />
import type { Machine, MachineMetrics } from "../../../../sdk/typescript/src/index"
import { machine as machinePath } from "../../../../sdk/typescript/src/index"
import { Callout, label, time, useRemote } from "../../common"
import styles from "./detail.module.css"

const units = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 })

function measure(raw: number | null | undefined, unit: string) {
  return raw != null && Number.isFinite(raw) ? units.format(raw) + " " + unit : undefined
}

export function ResourceUsage({ machine, revision }: { machine: Machine; revision: number }) {
  const { data, error } = useRemote<MachineMetrics>(machinePath(machine.machine_id) + "/metrics", revision)
  const execution = data?.execution
  const oldExecution = execution && (execution.binding_id !== machine.binding?.binding_id ||
    execution.binding_generation !== machine.binding_generation ||
    execution.incarnation_id !== machine.binding?.incarnation_id ||
    execution.execution_epoch !== machine.binding?.execution_epoch)
  const status = oldExecution ? "stale" : data?.availability ??
    (data && !machine.binding?.runtime_bundle_id ? "available" : undefined)
  const sample = status === "available" || status === "partial" ? data : undefined
  const metrics = [
    { name: "CPU", value: measure(sample?.cpu_millis, "ms"), description: "Cumulative CPU time" },
    { name: "Memory", value: measure(sample?.memory_mb, "MB"), description: "Runtime memory measurement" },
    { name: "Disk", value: measure(sample?.disk_used_mb, "MB"), description: "Runtime disk measurement" },
  ]
  return <section aria-labelledby="machine-usage-title">
    <div className={styles.metricsHead}>
      <h2 className={styles.sectionTitle} id="machine-usage-title">Resource usage</h2>
      <span>{data ? `${label(status ?? "unknown")}${data.source ? " · " + label(data.source) : ""}${data.scope ? " · " + label(data.scope) : ""}` : error ? "Unavailable" : "Loading…"}</span>
    </div>
    <Callout error={error} />
    <div className={styles.metrics}>{metrics.map(metric => <div className={styles.metric} key={metric.name}>
      <h3>{metric.name}</h3>
      <strong>{metric.value ?? (status === "stale" ? "Stale" : "Unavailable")}</strong>
      <span>{metric.description}</span>
      <div className={styles.metricGrid} aria-hidden="true" />
    </div>)}</div>
    <p className={styles.note}>{data?.observed_at || data?.at ? "Observed " + time(data.observed_at ?? data.at) : null}
      {oldExecution ? " · This observation belongs to an earlier execution." : null}
    </p>
    {Object.entries(data?.issues ?? {}).map(([key, issue]) => <p className={styles.note} key={key}>{issue.message}</p>)}
  </section>
}
