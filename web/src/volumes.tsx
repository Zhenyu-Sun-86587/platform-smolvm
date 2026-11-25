import { useState } from "react"
import type { Machine, RuntimeProfile, Volume, VolumeAttachment } from "../../sdk/typescript/src/index"
import { segment, useCollection } from "./api"
import { api, Callout, machinePowerState, num, Section, State, str, useRemote } from "./common"
import { PageIntro } from "./daytona/page"
import type { Actions } from "./main"

const capacityMiB = 1024 * 1024
const size = (bytes: number | undefined) => bytes === undefined ? "—" : `${(bytes / capacityMiB).toLocaleString()} MiB`

export function VolumeCatalog({ machines, actions, revision }: {
  machines?: Machine[]
  actions: Actions
  revision: number
}) {
  const [selected, select] = useState("")
  const [consumer, setConsumer] = useState("")
  const catalog = useCollection<Volume>("volumes", revision)
  const profiles = useCollection<RuntimeProfile>("runtime-profiles", revision)
  const path = selected ? "volumes/" + segment(selected) : ""
  const details = useRemote<Volume>(path, revision)
  const current = details.data
  const runtimeProfiles = profiles.items?.filter(item => item.status === "registered") ?? []
  const eligible = machines?.filter(machine => machine.boot_mode === "system" && machine.binding?.runtime_instance_id) ?? []
  const machine = eligible.find(item => item.machine_id === consumer)
  const machinePath = machine ? "machines/" + segment(machine.machine_id) : ""
  const leases = useCollection<VolumeAttachment & { volume_id: string }>(machine ? machinePath + "/volume-attachments" : "", revision)
  const stopped = machine && machinePowerState(machine) === "stopped" && !machine.operation_id

  const create = () => actions.dialog({
    title: "Create volume",
    fields: [
      { name: "name", label: "Name", required: true },
      { name: "capacity", label: "Capacity (MiB)", type: "number", min: 1, value: 1024, required: true },
      { name: "worker", label: "Worker", required: true, value: runtimeProfiles[0]?.runtime_instance_id,
        options: runtimeProfiles.map(item => ({ value: item.runtime_instance_id })) },
    ],
    act: form => actions.mutate("volumes", {
      name: str(form, "name"), capacity_bytes: num(form, "capacity") * capacityMiB, runtime_instance_id: str(form, "worker"),
    }),
  })

  const attach = (selectedVolume: Volume) => {
    if (!machine || !stopped) return
    const attachments = selectedVolume.snapshots.map(snapshot => ({ value: snapshot.snapshot_id }))
    actions.dialog({
      title: "Reserve volume attachment",
      description: "The Worker makes the device available on the next cold start. Mount it inside the Guest.",
      fields: [
        { name: "device", label: "Device key", value: "data", required: true },
        { name: "snapshot", label: "Access", value: "", options: [{ value: "", label: "Writable head" }, ...attachments] },
      ],
      act: async form => {
        const [volume, target] = await Promise.all([api.getVolume(selectedVolume.volume_id), api.getMachine(machine.machine_id)])
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
    <PageIntro title="Volumes" actions={<button className="primary" onClick={create} disabled={!runtimeProfiles.length}>Create volume</button>} />
    <Callout error={profiles.error} />
    <Section title="Registered volumes">
      <Callout error={catalog.error} />
      {!catalog.items?.length && !catalog.error && <p className="empty">{catalog.items ? "No volumes yet." : "Loading volumes…"}</p>}
      <div className="rows">{catalog.items?.map(volume => <div className="row" key={volume.volume_id}>
        <div><strong>{volume.name}</strong><small>{volume.volume_id}</small></div>
        <State value={volume.state} /><span>{size(volume.capacity_bytes)}</span>
        <button aria-pressed={selected === volume.volume_id} onClick={() => select(volume.volume_id)}>Details</button>
      </div>)}</div>
    </Section>
    <Callout error={details.error} />
    {current && <Section title={current.name} actions={<>
      <button onClick={() => actions.dialog({
        title: "Grow volume",
        description: "Expands the block device. Resize the filesystem inside the Guest afterward.",
        fields: [{ name: "capacity", label: "New capacity (MiB)", type: "number",
          min: Math.floor(current.capacity_bytes / capacityMiB) + 1, required: true }],
        act: form => actions.mutate(path, { capacity_bytes: num(form, "capacity") * capacityMiB }, { method: "PATCH", revision: current.revision }),
      })}>Grow</button>
      <button onClick={() => actions.dialog({
        title: "Capture volume snapshot",
        act: () => actions.mutate(path + "/snapshots", {}, { revision: current.revision }),
      })}>Snapshot</button>
      <button className="quiet-danger" onClick={() => actions.dialog({
        title: "Delete volume", destructive: true,
        act: async () => { await actions.mutate(path, {}, { method: "DELETE", revision: current.revision }); select("") },
      })}>Delete</button>
    </>}>
      {current.snapshots.length === 0 && <p className="empty">No volume snapshots yet.</p>}
      <div className="rows">{current.snapshots.map(snapshot => <div className="row" key={snapshot.snapshot_id}>
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
    <Section title="Machine attachments" actions={<>
      <select aria-label="Machine" value={consumer} onChange={event => setConsumer(event.target.value)}>
        <option value="">Select machine</option>
        {eligible.map(item => <option key={item.machine_id} value={item.machine_id}>{item.name}</option>)}
      </select>
      <button disabled={!current || !stopped || current.runtime_instance_id !== machine?.binding?.runtime_instance_id}
        onClick={() => current && attach(current)}>Attach selected volume</button>
    </>}>
      <Callout error={leases.error} />
      {machine && <State value={machinePowerState(machine)} />}
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
    </Section>
  </>
}
