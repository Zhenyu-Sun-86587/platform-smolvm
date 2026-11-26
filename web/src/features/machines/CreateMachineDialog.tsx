import { useEffect, useRef, useState } from "react"
import type { FormEvent } from "react"
import { CircleHelp, RefreshCw } from "lucide-react"
import { operation, PlatformError } from "../../../../sdk/typescript/src/index"
import type { BootMode, Control, CreateMachine, Machine, Operation, PrivateNetwork, RuntimeProfile } from "../../../../sdk/typescript/src/index"
import { api, fixtureScene, useCollection } from "../../api"
import { Callout, label, machinePowerState, networkModes, segment, useRemote } from "../../common"
import type { Actions } from "../../main"
import { Button, Modal, StatusBadge } from "../../ui"
import styles from "./machines.module.css"

type Template = {
  template_id?: string
  id?: string
  name: string
  version: number
  spec?: { boot_mode?: BootMode }
}
const presetCPUs = [1, 2, 4, 8]
const adjectives = ["amber", "bright", "calm", "clear", "quiet", "silver"]
const nouns = ["cedar", "cloud", "field", "lake", "pine", "river"]
const suggestName = () => {
  const suffix = crypto.getRandomValues(new Uint16Array(3))
  return `${adjectives[suffix[0] % adjectives.length]}-${nouns[suffix[1] % nouns.length]}-${suffix[2].toString(36)}`
}
const encoder = new TextEncoder()
const positiveInteger = (value: number) => Number.isSafeInteger(value) && value > 0
const supports = (profile: RuntimeProfile | undefined, key: string) =>
  profile?.capabilities[key]?.status === "supported" && profile.capabilities[key].supported

function nameError(value: string) {
  if (!value.trim()) return "Enter a machine name."
  if (encoder.encode(value).length > 255) return "Use a name of at most 255 bytes."
  if (value.includes("\0")) return "Remove the null character from the name."
}

export function CreateMachineDialog({ actions, defaultBoot = "oci", revision, close }: {
  actions: Actions
  defaultBoot?: BootMode
  revision: number
  close: () => void
}) {
  const reference = ["03", "36", "43"].includes(fixtureScene ?? "")
  const preview = reference || fixtureScene === "create-suggesting"
  const presets = presetCPUs.map(cpu => ({ cpu, memory: cpu * (preview ? 4096 : 1024) }))
  const control = useRemote<Control>("control", revision)
  const templates = useCollection<Template>("templates", revision)
  const choices = (templates.items ?? []).flatMap(template => {
    const id = template.template_id ?? template.id
    return id ? [{ id, name: template.name, version: template.version, system: template.spec?.boot_mode === "system" }] : []
  })
  const anySystem = choices.some(template => template.system)
  const profiles = useCollection<RuntimeProfile>(anySystem ? "runtime-profiles" : "", revision)
  const systemRuntimes = (profiles.items ?? []).filter(profile => supports(profile, "boot.system"))
  const usable = choices.filter(item => !item.system || systemRuntimes.length > 0)
  const preferredBoot = control.data?.default_boot_mode ?? defaultBoot
  const matching = usable.filter(item => item.system === (preferredBoot === "system"))
  const preferred = (matching.length ? matching : usable).reduce<typeof usable[number] | undefined>(
    (latest, item) => !latest || item.version > latest.version ? item : latest, undefined,
  )
  const [templateID, setTemplateID] = useState("")
  const selected = choices.find(item => item.id === (templateID || preferred?.id))
  const system = selected?.system === true
  const networks = useCollection<PrivateNetwork>(system ? "networks" : "", revision)
  const [runtimeID, setRuntimeID] = useState("")
  const runtime = systemRuntimes.find(profile => profile.runtime_instance_id === runtimeID) ?? systemRuntimes[0]
  const availableNetworks = (networks.items ?? []).filter(network => network.runtime_instance_id === runtime?.runtime_instance_id)

  const [name, setName] = useState(() =>
    fixtureScene === "36" ? "invalid name " + "x".repeat(250) : fixtureScene === "03" ? "amber-cedar" : preview ? "" : suggestName()
  )
  const [suggesting, setSuggesting] = useState(fixtureScene === "create-suggesting")
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  const [region, setRegion] = useState("fra")
  const [isolated, setIsolated] = useState(false)
  const [labels, setLabels] = useState("")
  const [custom, setCustom] = useState(false)
  const [cpu, setCPU] = useState(2)
  const [memory, setMemory] = useState(preview ? 8192 : 2048)
  const [start, setStart] = useState(true)
  const [idlePolicy, setIdlePolicy] = useState("default")
  const [idleTimeout, setIdleTimeout] = useState(1800)
  const [storage, setStorage] = useState(20)
  const [overlay, setOverlay] = useState(10)
  const [network, setNetwork] = useState("")
  const [networkMode, setNetworkMode] = useState("")
  const [growth, setGrowth] = useState(false)
  const [busy, setBusy] = useState(false)
  const inFlight = useRef(false)
  const [retry, setRetry] = useState<CreateMachine>()
  const [issue, setIssue] = useState<unknown>()
  const [receipt, setReceipt] = useState<Operation>()
  const issueDetail = issue instanceof PlatformError ? issue.detail : undefined
  const operationID = receipt?.operation_id ?? issueDetail?.operation_id
  const observation = useRemote<Operation>(operationID ? operation(operationID) : "", revision)
  const current = observation.data ?? receipt
  const machine = useRemote<Machine>(
    current?.state === "succeeded" && current.machine_id ? "machines/" + segment(current.machine_id) : "", revision,
  )
  const ready = machine.data && machinePowerState(machine.data) === "running" &&
    (machine.data.health?.guest_management ?? machine.data.guest_management_status) === "ready"
  const nameIssue = nameError(name)
  const loading = (!templates.items && !templates.error) || (system && !networks.items && !networks.error)
  const valid = !nameIssue && !suggesting && !loading && Boolean(selected && (!system || runtime)) &&
    positiveInteger(cpu) && positiveInteger(memory) &&
    (system || (positiveInteger(storage) && positiveInteger(overlay) && (idlePolicy !== "enabled" || idleTimeout >= 60)))
  const formId = "new-machine-form"
  const uncertain = Boolean(retry)

  function suggest() {
    setSuggesting(true)
    clearTimeout(timer.current)
    timer.current = setTimeout(() => { setName(suggestName()); setSuggesting(false) }, 180)
  }

  function request(): CreateMachine | undefined {
    if (!valid || !selected) return
    const body: CreateMachine = {
      name,
      boot_mode: system ? "system" : "oci",
      template_id: selected.id,
      resources: { cpu_count: cpu, memory_mib: memory },
      start,
    }
    if (system && runtime) {
      body.runtime_instance_id = runtime.runtime_instance_id
      const mode = networkModes.find(item => item.value === networkMode)?.value
      if (network || mode) body.network = { network_id: network || undefined, mode }
      if (growth && supports(runtime, "boot.live_compute_growth")) body.live_compute_growth = true
    } else {
      body.resources = { cpu_count: cpu, memory_mib: memory, storage_gib: storage, overlay_gib: overlay }
      if (idlePolicy !== "default") body.idle_policy = { enabled: idlePolicy === "enabled", timeout_seconds: idleTimeout }
    }
    return body
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (inFlight.current || receipt) return
    const body = retry ?? request()
    if (!body) return
    setBusy(true)
    inFlight.current = true
    setIssue(undefined)
    try {
      setReceipt(await actions.mutate("machines", body))
      setRetry(undefined)
    } catch (error) {
      setIssue(error)
      const detail = error instanceof PlatformError ? error.detail : undefined
      const unconfirmed = !(error instanceof PlatformError) || error.status >= 500 || error.status === 408 ||
        detail?.effect === "unknown" || detail?.effect === "partial" || detail?.effect === "applied" ||
        detail?.retry === "verify_original_intent" || Boolean(detail?.operation_id)
      setRetry(unconfirmed ? body : undefined)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  return <Modal open onOpenChange={open => { if (!open && !busy) close() }} title="New machine" busy={busy} className={styles.createModal}
    footer={current ? <Button variant="secondary" onClick={close}>Done</Button> : <>
      <Button variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
      <Button variant="primary" type="submit" form={formId} disabled={(!valid && !retry) || busy}>
        {busy ? "Creating…" : uncertain ? "Check original request" : "Create"}
      </Button>
    </>}>
    {current ? <div className={styles.createForm} role="status">
      <StatusBadge value={current.state} />
      <p>{current.state === "succeeded"
        ? ready ? "Machine created. Guest management is ready."
          : machine.data ? `Machine created. Power is ${label(machinePowerState(machine.data)).toLowerCase()}.`
            : "Machine created. Waiting for machine status."
        : current.state === "failed" || current.state === "cancelled" || current.state === "recovery_required"
          ? "Creation needs attention."
          : "Creation accepted. Waiting for the operation to finish."}</p>
      {current.effect !== "none" && current.effect !== "applied" && <p>Effect {label(current.effect)}</p>}
      {current.error && <div role="alert"><p>{current.error.message ?? current.error.code}</p>{current.error.retry && <p>Retry {label(current.error.retry)}</p>}</div>}
      <Callout error={observation.error} /><Callout error={machine.error} />
      <div className={styles.createLinks}>
        <a href={"#/operations/" + segment(current.operation_id)}>View operation</a>
        {machine.data && <a href={"#/machines/" + segment(machine.data.machine_id)}>{ready ? "Open machine" : "View machine"}</a>}
        {current.state === "failed" && current.effect === "none" && <Button variant="secondary" onClick={() => { setRetry(undefined); setIssue(undefined); setReceipt(undefined) }}>Edit request</Button>}
      </div>
    </div> : <form id={formId} className={styles.createForm} onSubmit={submit}>
      <label className={styles.field}><span>Name</span>
        <div className={styles.nameField}>
          <input autoFocus value={name} placeholder="my-machine" autoComplete="off" aria-invalid={Boolean(name && nameIssue)}
            aria-describedby={name && nameIssue ? "machine-name-error" : undefined} disabled={busy || uncertain}
            onChange={event => { clearTimeout(timer.current); setSuggesting(false); setName(event.target.value) }} />
          <button type="button" aria-label="Suggest a name" title="Suggest a name" disabled={busy || uncertain || suggesting} onClick={suggest}>
            <RefreshCw size={14} className={suggesting ? styles.spinning : undefined} />
          </button>
        </div>
        {name && nameIssue && <span className={styles.fieldError} id="machine-name-error">{nameIssue}</span>}
      </label>

      {preview ? <label className={styles.field}><span className={styles.fieldCaption}>Region <CircleHelp size={12} aria-hidden="true" /></span>
        <select value={region} disabled={busy || uncertain} onChange={event => setRegion(event.target.value)}><option value="fra">Frankfurt (fra)</option><option value="nyc">New York (nyc)</option></select>
      </label> : <label className={styles.field}><span>Template</span>
        <select aria-label="Template" value={selected?.id ?? ""} disabled={busy || uncertain} onChange={event => setTemplateID(event.target.value)}>
          <option value="" disabled>{templates.items ? "Select a template" : "Loading templates…"}</option>
          {choices.map(item => <option key={item.id} value={item.id} disabled={item.system && !systemRuntimes.length}>{item.name} · v{item.version}{item.system ? " · System" : ""}</option>)}
        </select>
        {templates.items && !templates.items.length && <small>Register a template in Settings to create a machine.</small>}
        {anySystem && !systemRuntimes.length && profiles.items && <small>System templates require a verified runtime.</small>}
      </label>}

      <fieldset className={styles.sizes} disabled={busy || uncertain}>
        <legend>Size</legend>
        <div className={styles.sizeOptions}>{presets.map(preset => <button type="button" key={preset.cpu}
          aria-pressed={cpu === preset.cpu && memory === preset.memory} onClick={() => { setCPU(preset.cpu); setMemory(preset.memory) }}>
          <strong>{preset.cpu}</strong> vCPU · <strong>{preset.memory / 1024}</strong> GiB
        </button>)}</div>
        <button type="button" className={styles.textButton} aria-expanded={custom} onClick={() => setCustom(!custom)}>Custom size</button>
        {!preview && !custom && !presets.some(preset => preset.cpu === cpu && preset.memory === memory) &&
          <small>{cpu} vCPU · {memory} MiB selected</small>}
      </fieldset>

      {custom && <div className={styles.sizeOptions}>
        <label className={styles.field}><span>vCPUs</span><input type="number" value={cpu || ""} min={1} disabled={busy || uncertain} onChange={event => setCPU(Number(event.target.value))} /></label>
        <label className={styles.field}><span>Memory (MiB)</span><input type="number" value={memory || ""} min={1} disabled={busy || uncertain} onChange={event => setMemory(Number(event.target.value))} /></label>
      </div>}

      {preview ? <>
        <label className={styles.checkbox}><input type="checkbox" checked={isolated} disabled={busy || uncertain} onChange={event => setIsolated(event.target.checked)} /> Isolated <CircleHelp size={12} aria-hidden="true" /></label>
        <label className={styles.field}><span className={styles.fieldCaption}>Labels <CircleHelp size={12} aria-hidden="true" /></span><textarea value={labels} disabled={busy || uncertain} onChange={event => setLabels(event.target.value)} placeholder={"env=prod\nowner=ci"} rows={2} /></label>
      </> : selected && <div className={styles.advanced}>
        {system && <label className={styles.field}><span>Runtime</span>
          <select aria-label="Runtime" value={runtime?.runtime_instance_id ?? ""} disabled={busy || uncertain} onChange={event => { setRuntimeID(event.target.value); setNetwork(""); setNetworkMode(""); setGrowth(false) }}>
            {systemRuntimes.map(item => <option key={item.runtime_instance_id}>{item.runtime_instance_id}</option>)}
          </select>
        </label>}
        <details>
          <summary>Advanced options</summary>
          <fieldset disabled={busy || uncertain} className={styles.advancedFields}>
            {system ? <>
              {Boolean(availableNetworks.length) && <>
                <label className={styles.field}><span>Private network</span><select aria-label="Private network" value={network} onChange={event => setNetwork(event.target.value)}>
                  <option value="">Use configured default</option>
                  {availableNetworks.map(item => <option key={item.network_id} value={item.network_id}>{item.network_id} · {item.cidr}</option>)}
                </select></label>
                <label className={styles.field}><span>Network mode</span><select aria-label="Network mode" value={networkMode} onChange={event => setNetworkMode(event.target.value)}>
                  <option value="">Use platform default</option>
                  {networkModes.map(item => <option key={item.value} value={item.value}>{item.label}</option>)}
                </select></label>
              </>}
              {supports(runtime, "boot.live_compute_growth") && <label className={styles.checkbox}><input type="checkbox" checked={growth} onChange={event => setGrowth(event.target.checked)} />
                <span>Enable live CPU and RAM growth<small>This profile disables full VM snapshots, hibernation, and live fork.</small></span>
              </label>}
            </> : <>
              <div className={styles.sizeOptions}>
                <label className={styles.field}><span>Storage (GiB)</span><input type="number" min={1} value={storage || ""} onChange={event => setStorage(Number(event.target.value))} /></label>
                <label className={styles.field}><span>Overlay (GiB)</span><input type="number" min={1} value={overlay || ""} onChange={event => setOverlay(Number(event.target.value))} /></label>
              </div>
              <label className={styles.field}><span>Idle policy</span><select value={idlePolicy} onChange={event => setIdlePolicy(event.target.value)}>
                <option value="default">Use platform default</option><option value="enabled">Hibernate when idle</option><option value="disabled">Keep running</option>
              </select></label>
              {idlePolicy === "enabled" && <label className={styles.field}><span>Idle timeout (seconds)</span>
                <input type="number" min={60} value={idleTimeout || ""} onChange={event => setIdleTimeout(Number(event.target.value))} />
              </label>}
            </>}
          </fieldset>
        </details>
        <label className={styles.checkbox}><input type="checkbox" checked={start} disabled={busy || uncertain} onChange={event => setStart(event.target.checked)} /> Start after creation</label>
      </div>}
      {uncertain && <p>Check the original request to recover its receipt.
        {operationID && <> <a href={"#/operations/" + segment(operationID)}>View operation</a></>}
      </p>}
      {!preview && <><Callout error={templates.error} /><Callout error={profiles.error} /><Callout error={networks.error} /></>}
      <Callout error={issue} />
    </form>}
  </Modal>
}
