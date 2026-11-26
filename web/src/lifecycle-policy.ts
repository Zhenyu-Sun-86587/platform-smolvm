import type { LifecyclePolicy, LifecyclePolicyPatch } from "../../sdk/typescript/src/index"
import type { Field } from "./common"
import { num, str } from "./common"

export function policyFields(p: LifecyclePolicy): Field[] {
  return [
    { name: "auto_standby", label: "Enable automatic standby", type: "checkbox", value: p.auto_standby },
    { name: "standby_after_seconds", label: "Standby after idle seconds", type: "number", min: 0, required: true, value: p.standby_after_seconds },
    { name: "auto_hibernate", label: "Enable automatic hibernation", type: "checkbox", value: p.auto_hibernate },
    { name: "hibernate_after_seconds", label: "Hibernate after idle seconds", type: "number", min: 0, required: true, value: p.hibernate_after_seconds },
    { name: "keep_running", label: "Keep running", type: "checkbox", value: p.keep_running },
    { name: "reclaim_priority", label: "Reclaim priority (0 is reclaimed first, 100 last)", type: "number", min: 0, max: 100, required: true, value: p.reclaim_priority },
    { name: "pressure_reclaim", label: "Memory pressure", value: p.pressure_reclaim,
      options: [{ value: "idle_only", label: "Reclaim idle instances" }, { value: "disabled", label: "Exclude from pressure reclaim" }] },
    { name: "min_running_after_wake_seconds", label: "Minimum running seconds after wake", type: "number", min: 0, value: p.min_running_after_wake_seconds, required: true },
    { name: "retry_backoff_seconds", label: "Retry interval (seconds)", type: "number", min: 1, value: p.retry_backoff_seconds, required: true },
    { name: "api_access", label: "Wake on authorized API access", type: "checkbox", value: p.wake_conditions.api_access },
    { name: "service_access", label: "Wake on authorized service access", type: "checkbox", value: p.wake_conditions.service_access },
    { name: "private_network_packet", label: "Wake on an authorized private packet", type: "checkbox", value: p.wake_conditions.private_network_packet },
    { name: "allow_cold_start", label: "Allow access to start a stopped instance", type: "checkbox", value: p.wake_conditions.allow_cold_start },
  ]
}

export function policyInput(form: FormData): LifecyclePolicyPatch {
  return {
    auto_standby: form.has("auto_standby"), standby_after_seconds: num(form, "standby_after_seconds"),
    auto_hibernate: form.has("auto_hibernate"), hibernate_after_seconds: num(form, "hibernate_after_seconds"),
    keep_running: form.has("keep_running"), reclaim_priority: num(form, "reclaim_priority"),
    pressure_reclaim: str(form, "pressure_reclaim") === "idle_only" ? "idle_only" : "disabled",
    min_running_after_wake_seconds: num(form, "min_running_after_wake_seconds"), retry_backoff_seconds: num(form, "retry_backoff_seconds"),
    wake_conditions: { api_access: form.has("api_access"), service_access: form.has("service_access"),
      private_network_packet: form.has("private_network_packet"), allow_cold_start: form.has("allow_cold_start") },
  }
}

export function policyOverrides(form: FormData, current: LifecyclePolicy): LifecyclePolicyPatch {
  const { wake_conditions, ...settings } = policyInput(form)
  const overrides = Object.fromEntries(Object.entries(settings).filter(([key, value]) =>
    value !== current[key as keyof LifecyclePolicy]))
  const wake = Object.fromEntries(Object.entries(wake_conditions ?? {}).filter(([key, value]) =>
    value !== current.wake_conditions[key as keyof LifecyclePolicy["wake_conditions"]]))
  return Object.keys(wake).length ? { ...overrides, wake_conditions: wake } : overrides
}
