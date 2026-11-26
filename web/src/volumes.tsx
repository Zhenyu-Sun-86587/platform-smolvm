import { useRef, useState } from "react"
import type { FormEvent } from "react"
import { HardDrive, Plus } from "lucide-react"
import type { Machine, RuntimeProfile, Volume, VolumeAttachment } from "../../sdk/typescript/src/index"
import { fixtureScene, segment, useCollection } from "./api"
import { api, Callout, machinePowerState, num, Section, State, str, useRemote } from "./common"
import { DocumentationLink } from "./features/catalog/DocumentationLink"
import { Button, Modal, PageHeader } from "./ui"
import styles from "./features/catalog/catalog.module.css"
import type { Actions } from "./main"

const capacityMiB = 1024 * 1024
const capacityGiB = 1024 * capacityMiB
const size = (bytes: number | undefined) => bytes === undefined ? "—"
  : bytes >= capacityGiB ? `${(bytes / capacityGiB).toLocaleString()} GiB` : `${(bytes / capacityMiB).toLocaleString()} MiB`

export function VolumeCatalog({ machines, actions, revision }: {
  machines?: Machine[]
  actions: Actions
  revision: number
}) {
  const [selected, select] = useState("")
  const [creating, setCreating] = useState(fixtureScene === "17")
  const demo = fixtureScene === "06" || fixtureScene === "17"
  const [region, setRegion] = useState("fra")
  const [consumer, setConsumer] = useState("")
  const catalog = useCollection<Volume>("volumes", revision)
  const profiles = useCollection<RuntimeProfile>("runtime-profiles", revision)
  const path = selected ? "volumes/" + segment(selected) : ""
  const details = useRemote<Volume>(path, revision)
  const current = details.data
  const snapshots = current?.snapshots ?? []
  const runtimeProfiles = profiles.items?.filter(item => item.status === "registered") ?? []
  const eligible = machines?.filter(machine => machine.boot_mode === "system" && machine.binding?.runtime_instance_id) ?? []
  const machine = eligible.find(item => item.machine_id === consumer)
  const machinePath = machine ? "machines/" + segment(machine.machine_id) : ""
  const leases = useCollection<VolumeAttachment & { volume_id: string }>(machine ? machinePath + "/volume-attachments" : "", revision)
  const stopped = machine && machinePowerState(machine) === "stopped" && !machine.operation_id

  const attach = (selectedVolume: Volume) => {
    if (!machine || !stopped) return
    const attachments = (selectedVolume.snapshots ?? []).map(snapshot => ({ value: snapshot.snapshot_id }))
    actions.dialog({
      title: "Reserve volume attachment",
      description: "The Worker makes the device available on the next cold start. Mount it inside the Guest.",
      fields: [
        { name: "device", label: "Device key", value: "data", required: true },
        { name: "snapshot", label: "Access", value: "", options: [{ value: "", label: "Writable head" }, ...attachments] },
      ],
      act: async form => {
        const [volume, target] = await Promise.all([api.getVolume(selectedVolume.volume_id), api.getMachine(machine.machine_id)])
        if (machinePowerState(target) !== "stopped" || target.operation_id || target.binding?.runtime_instance_id !== volume.runtime_instance_id) {
          throw new Error("The machine no longer has an available attachment slot on this worker.")
        }
        const snapshot = str(form, "snapshot")
        return actions.mutate(machinePath + "/volume-attachments", {
          volume_id: volume.volume_id, device_key: str(form, "device"), mode: snapshot ? "ro" : "rw",
          ...(snapshot ? { snapshot_id: snapshot } : {}),
          expected_volume_revision: volume.revision, expected_generation: target.binding_generation,
        }, { revision: target.revision })
      },
    })
  }

  return <>
    <PageHeader title="Disks" icon={<HardDrive />} description="Attach persistent storage to machines on its owning Worker." className={styles.stackedHeading}
      actions={<><Button variant="primary" onClick={() => setCreating(true)} disabled={!runtimeProfiles.length && !demo}><Plus size={15} /> New disk</Button><DocumentationLink icon={false} /></>} />
    <Callout error={profiles.error} />
    <Callout error={catalog.error} />
    {!demo && profiles.items && !runtimeProfiles.length && <p role="status">Creating disks requires a registered worker.</p>}
    {demo && <div className={styles.toolbar}><div className={styles.actions}><select className={styles.regionSelect} aria-label="Region" value={region} onChange={event => setRegion(event.target.value)}>
      <option value="fra">Frankfurt</option><option value="nyc">New York</option>
    </select></div></div>}
    {!catalog.items && !catalog.error && <div className={styles.skeleton} role="status" aria-label="Loading disks" />}
    {catalog.items?.length === 0 && <p className={styles.empty}>{demo ? `No disks in ${region === "fra" ? "Frankfurt" : "New York"}.` : "No disks yet."}</p>}
    {!!catalog.items?.length && <div className={styles.tableFrame}><table className={styles.table}>
      <thead><tr>{["Disk", "State", "Capacity", "Worker", "Actions"].map(column => <th scope="col" key={column}>{column}</th>)}</tr></thead>
      <tbody>{catalog.items.map(volume => <tr key={volume.volume_id}>
        <td><strong>{volume.name}</strong><small>{volume.volume_id}</small></td>
        <td><State value={volume.state} /></td><td>{size(volume.capacity_bytes)}</td><td><code>{volume.worker_id ?? volume.runtime_instance_id}</code></td>
        <td><Button size="sm" variant="secondary" aria-expanded={selected === volume.volume_id}
          onClick={() => select(selected === volume.volume_id ? "" : volume.volume_id)}>Details</Button></td>
      </tr>)}</tbody>
    </table></div>}
    <Callout error={details.error} />
    {path && !current && !details.error && <div className={styles.skeleton} role="status" aria-label="Loading disk details" />}
    {current && <Section title={current.name} actions={<>
      <button disabled={current.state !== "available"} onClick={() => actions.dialog({
        title: "Grow volume",
        description: "Expands the block device. Resize the filesystem inside the Guest afterward.",
        fields: [{ name: "capacity", label: "New capacity (MiB)", type: "number",
          min: Math.floor(current.capacity_bytes / capacityMiB) + 1, required: true }],
        act: form => actions.mutate(path, { capacity_bytes: num(form, "capacity") * capacityMiB }, { method: "PATCH", revision: current.revision }),
      })}>Grow</button>
      <button disabled={current.state !== "available"} onClick={() => actions.dialog({
        title: "Capture volume snapshot",
        act: () => actions.mutate(path + "/snapshots", {}, { revision: current.revision }),
      })}>Snapshot</button>
      <button className="quiet-danger" disabled={current.state !== "available"} onClick={() => actions.dialog({
        title: "Delete volume", destructive: true,
        act: async () => { await actions.mutate(path, {}, { method: "DELETE", revision: current.revision }); select("") },
      })}>Delete</button>
    </>}>
      {snapshots.length === 0 && <p className="empty">No volume snapshots yet.</p>}
      <div className="rows">{snapshots.map(snapshot => <div className="row" key={snapshot.snapshot_id}>
        <div><strong>{snapshot.snapshot_id}</strong><small>{snapshot.generation_id}</small></div>
        <span>{size(snapshot.capacity_bytes)}</span>
        <button onClick={() => actions.dialog({
          title: "Clone volume snapshot", fields: [{ name: "name", label: "New volume name", required: true }],
          act: form => actions.mutate("volume-snapshots/" + segment(snapshot.snapshot_id) + "/clones", { name: str(form, "name") }),
        })}>Clone</button>
        <button className="quiet-danger" onClick={() => actions.dialog({
          title: "Delete volume snapshot", destructive: true,
          act: () => actions.mutate("volume-snapshots/" + segment(snapshot.snapshot_id), {}, { method: "DELETE", revision: current.revision }),
        })}>Delete</button>
      </div>)}</div>
    </Section>}
    {eligible.length > 0 && !demo && <Section title="Machine attachments" actions={<>
      <select aria-label="Machine" value={consumer} onChange={event => setConsumer(event.target.value)}>
        <option value="">Select machine</option>
        {eligible.map(item => <option key={item.machine_id} value={item.machine_id}>{item.name}</option>)}
      </select>
      <button disabled={!current || current.state !== "available" || !stopped || current.runtime_instance_id !== machine?.binding?.runtime_instance_id}
        onClick={() => current && attach(current)}>Attach selected volume</button>
    </>}>
      <Callout error={leases.error} />
      {machine && <State value={machinePowerState(machine)} />}
      {machine && !stopped && <p>Stop the machine before changing attachments.</p>}
      {machine && !leases.items && !leases.error && <div className={styles.skeleton} role="status" aria-label="Loading disk attachments" />}
      {machine && leases.items?.length === 0 && <p className="empty">No volumes reserved for this machine.</p>}
      <div className="rows">{leases.items?.map(lease => <div className="row" key={lease.attachment_id}>
        <div><strong>{lease.device_key}</strong><small>{catalog.items?.find(item => item.volume_id === lease.volume_id)?.name ?? lease.volume_id}</small></div>
        <span>{lease.mode === "ro" ? "Read only" : "Read and write"}</span><State value={lease.state} />
        <button className="quiet-danger" disabled={!stopped || !machine} onClick={() => machine && actions.dialog({
          title: "Detach volume", destructive: true,
          act: async () => {
            const [volume, target] = await Promise.all([api.getVolume(lease.volume_id), api.getMachine(machine.machine_id)])
            return actions.mutate("volume-attachments/" + segment(lease.attachment_id), {
              expected_volume_revision: volume.revision, expected_generation: target.binding_generation,
            }, { method: "DELETE", revision: target.revision })
          },
        })}>Detach</button>
      </div>)}</div>
    </Section>}
    {creating && <CreateDisk profiles={runtimeProfiles} demo={demo} actions={actions} close={() => setCreating(false)} />}
  </>
}

function CreateDisk({ profiles, demo, actions, close }: {
  profiles: RuntimeProfile[]; demo: boolean; actions: Actions; close: () => void
}) {
  const [name, setName] = useState(demo ? "my-data" : "")
  const [capacity, setCapacity] = useState(demo ? "10240" : "1024")
  const [worker, setWorker] = useState("")
  const [region, setRegion] = useState("fra")
  const runtime = profiles.find(profile => profile.runtime_instance_id === worker) ?? profiles[0]
  const [busy, setBusy] = useState(false)
  const inFlight = useRef(false)
  const [error, setError] = useState<unknown>()
  const nameValid = name === name.trim() && name.length > 0 && !/[\0\r\n]/.test(name) && new TextEncoder().encode(name).length <= 128
  const bytes = Number(capacity) * capacityMiB
  const valid = nameValid && Number.isSafeInteger(bytes) && bytes > 0 && bytes % 512 === 0 && Boolean(runtime)

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (inFlight.current || !valid || !runtime) return
    inFlight.current = true
    setBusy(true)
    setError(undefined)
    try {
      await actions.mutate("volumes", { name, capacity_bytes: bytes, runtime_instance_id: runtime.runtime_instance_id })
      close()
    } catch (reason) {
      setError(reason)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  return <Modal open busy={busy} onOpenChange={open => { if (!open) close() }} title="New disk" className={styles.smallModal}
    description="Create a disk on the Worker that will host its machines."
    footer={<><Button variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
      <Button variant="primary" type="submit" form="create-disk" disabled={busy || !valid}>{busy ? "Creating…" : "Create"}</Button></>}>
    <form id="create-disk" className={styles.form} onSubmit={submit}>
      <label>Name<input value={name} placeholder="my-data" aria-invalid={Boolean(name && !nameValid)}
        aria-describedby={name && !nameValid ? "disk-name-error" : undefined} disabled={busy} onChange={event => setName(event.target.value)} /></label>
      {name && !nameValid && <p id="disk-name-error" role="alert">Enter a name up to 128 bytes with no leading or trailing spaces or line breaks.</p>}
      {demo ? <>
        <label>Size<select value={capacity} disabled={busy} onChange={event => setCapacity(event.target.value)}>
          {[10, 20, 50, 100].map(gib => <option key={gib} value={gib * 1024}>{gib} GiB</option>)}
        </select></label>
        <label>Region<select value={region} disabled={busy} onChange={event => setRegion(event.target.value)}>
          <option value="fra">Frankfurt</option><option value="nyc">New York</option>
        </select></label>
      </> : <>
        <label>Size (MiB)<input type="number" min={1} value={capacity} disabled={busy} onChange={event => setCapacity(event.target.value)} /></label>
        <label>Worker<select value={runtime?.runtime_instance_id ?? ""} disabled={busy} onChange={event => setWorker(event.target.value)}>
          {profiles.map(profile => <option key={profile.runtime_instance_id} value={profile.runtime_instance_id}>{[profile.worker_id, profile.runtime_instance_id].filter(Boolean).join(" · ")}</option>)}
        </select></label>
      </>}
      <Callout error={error} />
    </form>
  </Modal>
}
