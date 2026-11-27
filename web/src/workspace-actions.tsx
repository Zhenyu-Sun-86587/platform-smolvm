import { useRef, useState } from "react"
import {
  Cable,
  Check,
  AppWindow,
  Box,
  CodeXml,
  ExternalLink,
  Feather,
  Smartphone,
  Copy,
  Ellipsis,
  GitFork,
  Layers2,
  Monitor,
  Moon,
  Play,
  Power,
  SquareTerminal,
  Waypoints,
  Trash2,
  Zap,
} from "lucide-react"
import type { Capability, Machine, Service } from "../../sdk/typescript/src/index"
import type { Actions } from "./main"
import { bundleBound, Callout, fullStateBlock, label, machinePowerState, segment, str } from "./common"
import type { Field } from "./common"
import { forkMemorySupport, memoryForkBackendLabel } from "./memory-fork"
import { Button } from "./daytona/button"
import { Modal } from "./ui"
import { desktopHref, machineHref, terminalHref } from "./routes"
import { fixtureScene, useCollection } from "./api"
import consoleStyles from "./features/console/console.module.css"
import { MachineWebAddress } from "./features/machine/MachineWebAddress"
import { PreviewQR } from "./features/console/PreviewQR"
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "./daytona/dropdown-menu"

export function powerDialog(m: Machine, actions: Actions, kind: string) {
  const deleting = kind === "delete"
  const managed = bundleBound(m)
  const captures = kind === "pause" || kind === "hibernate"
  actions.dialog({
    title: (kind === "stop" ? "Cold stop" : kind === "pause" ? "Hibernate" : label(kind)) + " machine",
    submit: captures ? "Hibernate" : label(kind),
    description:
      captures
        ? "Save the complete running state, including memory, for a later resume."
        : kind === "standby"
          ? "Suspend the execution in memory. The host keeps its current allocation."
        : kind === "stop"
          ? "This ends the running execution. The guest disk is retained."
          : deleting
            ? "Delete " + m.name + " and its runtime binding. Active dependencies can block deletion."
            : undefined,
    destructive: deleting || kind === "stop",
    fields:
      captures
        ? [{ name: "allow_active_tasks", label: "Include active persistent tasks", type: "checkbox" }]
        : kind === "stop" && !managed
          ? [{ name: "cancel_tasks", label: "Cancel active persistent tasks first", type: "checkbox" }]
          : [],
    act: (f) =>
      actions.mutate(
        "machines/" + segment(m.machine_id) + (deleting ? "" : "/" + kind),
        {
          expected_generation: m.binding_generation,
          ...(f.has("allow_active_tasks") ? (kind === "hibernate"
            ? { activity_policy: { allow_active_tasks: true } } : { allow_active_tasks: true }) : {}),
          ...(f.has("cancel_tasks") ? { cancel_tasks: true } : {}),
        },
        { method: deleting ? "DELETE" : "POST", revision: m.revision },
      ),
  })
}
function forkMemoryField(capability?: Capability): Field {
  const { available, observation, native, kernelCow } = forkMemorySupport(capability)
  let note = capability?.message || capability?.reason_code || "Fork has not been verified for this source."
  if (available) {
    if (!observation) note = "The Worker has not reported Kernel CoW support. Auto can use Native for this execution."
    else if (kernelCow) note = "Choose Kernel CoW or Auto to fork this execution."
    else if (native) note = "Kernel CoW unavailable. " + (observation.unavailable_reason || "This execution uses the Native RAM profile.")
    else note = observation.unavailable_reason || "This execution has not reported a supported RAM backend."
  }
  return {
    name: "memory_fork_backend",
    label: "Memory backend",
    required: !native,
    options: [
      { value: "", label: native ? "Use default (Native)" : "Choose a backend", disabled: !native },
      { value: "native", label: memoryForkBackendLabel("native"), disabled: !native },
      { value: "kernel_cow", label: memoryForkBackendLabel("kernel_cow"), disabled: !kernelCow },
      { value: "auto", label: memoryForkBackendLabel("auto"), disabled: !native && !kernelCow },
    ],
    note,
  }
}

function forkMemoryRequest(form: FormData) {
  const backend = str(form, "memory_fork_backend")
  return backend ? { memory_fork_backend: backend } : {}
}

export function copyDialog(m: Machine, actions: Actions, live: boolean, independent = false) {
  actions.dialog({
    title: independent ? "Independent Fork" : live ? "Live Fork" : "Independent Clone",
    submit: live ? "Create fork" : "Create clone",
    description: independent ? "Capture the running execution and fork a resource-independent child."
      : live
      ? "Branch the running execution. Parent and child continue from the same point with a native storage dependency."
      : "Capture a clean standalone checkpoint and restore it into a separate workspace.",
    fields: [
      {
        name: "child_name",
        label: "Child machine name",
        value: m.name + (live ? "-fork" : "-clone"),
        required: true,
      },
      ...(independent ? [forkMemoryField(m.capabilities?.["state.independent_fork"])] : []),
    ],
    act: (f) =>
      actions.mutate(
        "machines/" + segment(m.machine_id) + (live ? "/forks" : "/clones"),
        {
          child_name: str(f, "child_name"),
          expected_generation: m.binding_generation,
          ...(live ? {} : { copy_policy: "managed_clean" }),
          ...(independent ? { require_independent: true, ...forkMemoryRequest(f) } : {}),
        },
        { revision: m.revision },
      ),
  })
}

export function forkBatchDialog(m: Machine, actions: Actions) {
  actions.dialog({
    title: "Fork batch",
    description: "Create independent children from one source capture.",
    submit: "Fork batch",
    fields: [{ name: "children", label: "Child names, one per line", type: "textarea", required: true },
      forkMemoryField(m.capabilities?.["state.fork_batch"]),
      { name: "best_effort", label: "Create as many children as the Worker can admit", type: "checkbox" }],
    act: (f) => actions.mutate("machines/" + segment(m.machine_id) + "/fork-batches", {
      expected_generation: m.binding_generation,
      require_independent: true,
      ...forkMemoryRequest(f),
      ...(f.has("best_effort") ? { admission: "best_effort" } : {}),
      children: str(f, "children").split("\n").map(name => name.trim()).filter(Boolean).map(name => ({name})),
    }, { revision: m.revision }),
  })
}
export function hasManagedSSH(machine: Machine) {
  const access = machine.capabilities?.["access.managed_ssh"]
  return access?.status === "supported" && access.supported === true && access.constraints?.manage_allowed === true &&
    access.constraints.path === "configured"
}

export function hasVerifiedSSHHostKey(machine: Machine) {
  const evidence = machine.binding?.guest_management_evidence
  return evidence?.ssh_ready === true && Boolean(evidence.ssh_host_key?.trim())
}

function ConnectionInfo({ machine }: { machine: Machine }) {
  const { items, error } = useCollection<Service>("machines/" + segment(machine.machine_id) + "/services", machine.revision)
  const [copiedCommand, setCopiedCommand] = useState(""),
    [copyError, setError] = useState<unknown>()
  const service = items?.find((item) => item.protocol === "ssh" && item.enabled !== false)
  const cmd = "sandboxctl ssh-config " + machine.machine_id
  const hostKey = "sandboxctl ssh-host-key " + machine.machine_id
  const managed = hasManagedSSH(machine)
  const direct = hasVerifiedSSHHostKey(machine)
  const manual = hostKey + "\n" + cmd + "\nssh sandbox-" + machine.machine_id
  let commands = fixtureScene === "30" ? cmd : manual
  if (managed) commands = "sandboxctl ssh " + machine.machine_id
  else if (direct) commands = "sandboxctl ssh --legacy " + machine.machine_id
  const copied = commands === copiedCommand
  return <><section aria-label="SSH with sandboxctl">
    <div className={consoleStyles.connectionLabel}><h3>CLI</h3><span>Connect using OpenSSH</span></div>
    <Callout error={error || copyError} />
    {managed || service ? <>
      <div className={consoleStyles.commands}>
        <pre>{fixtureScene === "30" ? "$ " + commands : commands}</pre>
        <Button size="sm" variant="outline" className={consoleStyles.copyCommand} aria-label={copied ? "SSH commands copied" : "Copy SSH commands"}
          onClick={() => navigator.clipboard.writeText(commands).then(() => setCopiedCommand(commands)).catch(setError)}>
          {copied ? <><Check /> Copied</> : <><Copy /> Copy</>}
        </Button>
      </div>
      {managed ? <p className={consoleStyles.sshHint}>Managed SSH is configured. When you connect, the platform checks your permission to manage this machine and authorizes your SSH public key for this session.
        {machine.capabilities?.["access.managed_ssh"]?.constraints?.host_key_status === "observed" && " The platform has observed the host key for this execution."}
        {" "}After CLI login, OpenSSH and editors can use the generated <code>NAME.sandbox</code> alias for a unique eligible name.</p>
        : direct && <p className={consoleStyles.sshHint}>Use a public key authorized for your Guest SSH user.</p>}
      {service && (managed || direct) ? <details className={consoleStyles.sshHint}><summary>Manual OpenSSH setup</summary>
        <p>With an enrolled key, use <code>sandboxctl ssh --legacy {machine.machine_id}</code> or select one with <code>--identity FILE</code>.</p>
        <p>Add the output of <code>{hostKey}</code> to <code>~/.ssh/sandbox_known_hosts</code> and the output of <code>{cmd}</code> to your SSH configuration. Then run <code>ssh sandbox-{machine.machine_id}</code>.</p>
      </details> : !managed && !fixtureScene && <p className={consoleStyles.sshHint}>Add the verified host key to <code>~/.ssh/sandbox_known_hosts</code> and the generated Host entry to your SSH configuration.</p>}
    </> : !items && !error ? <p role="status">Checking published services…</p> : items ? <p className={consoleStyles.sshHint}><a href={"#/machines/" + segment(machine.machine_id) + "/networking"}>Publish an SSH service</a> for guest port 22 to connect.</p> : null}
  </section>
    <MachineWebAddress machine={machine} services={items} /></>
}

function useTerminalLaunch(machine: Machine, actions: Actions, onOpened?: () => void) {
  const [opening, setOpening] = useState(false)
  const pending = useRef(false)
  const [error, setError] = useState<unknown>()
  async function openTerminal() {
    if (pending.current) return
    pending.current = true
    setOpening(true)
    setError(undefined)
    try {
      const session = await actions.mutate<{ exec_id: string }>("machines/" + segment(machine.machine_id) + "/execs", {
        argv: ["/bin/sh"], cwd: machine.boot_mode === "system" && bundleBound(machine) ? "/" : "/workspace",
        user: "root", tty: true, interactive: true, cols: 100, rows: 28,
      }, { revision: machine.revision })
      if (!session?.exec_id) throw new Error("The terminal response did not include a session.")
      onOpened?.()
      location.hash = terminalHref(machine.machine_id, session.exec_id)
    } catch (error) {
      setError(error)
    } finally {
      pending.current = false
      setOpening(false)
    }
  }
  return { opening, error, openTerminal }
}

export function ConnectDialog({ machine, actions, open, onOpenChange }: {
  machine: Machine; actions: Actions; open: boolean; onOpenChange: (open: boolean) => void
}) {
  const { opening, error, openTerminal } = useTerminalLaunch(machine, actions, () => onOpenChange(false))
  const desktop = machine.desktop_profile === "software-vnc-v1"
  return <Modal open={open} onOpenChange={onOpenChange} busy={opening} size="md" className={consoleStyles.connectDialog}
    title={`Open ${machine.name}`} description="Pick how you want to get into this machine.">
    <div className={consoleStyles.connectBody}>
      <ConnectionInfo machine={machine} />
      <section aria-label="Web">
      <div className={consoleStyles.connectionLabel}><h3>Web</h3></div>
      <div className={consoleStyles.connectionLinks}>
        <Button className={consoleStyles.connectionLink} variant="outline" onClick={() => void openTerminal()} disabled={opening}>
          <i><SquareTerminal size={18}/></i><span><strong>{opening ? "Opening…" : "Web terminal"}</strong><small>Interactive guest shell</small></span>
        </Button>
        {desktop && <Button className={consoleStyles.connectionLink} variant="outline" asChild>
          <a href={desktopHref(machine.machine_id)} onClick={event => {
            if (opening) { event.preventDefault(); return }
            onOpenChange(false)
          }} aria-disabled={opening || undefined}>
            <i><Monitor size={18}/></i><span><strong>Desktop</strong><small>Guest display via VNC</small></span>
          </a>
        </Button>}
      </div>
      <Callout error={error} />
      </section>
      {fixtureScene === "30" ? <ConnectionPreview /> : <a className={consoleStyles.sessionsLink} href={machineHref(machine.machine_id, "terminal")} onClick={event => {
        if (opening) { event.preventDefault(); return }
        onOpenChange(false)
      }}><Waypoints size={15} /> Browse existing sessions</a>}
    </div>
  </Modal>
}

function ConnectionPreview() {
  const editors = [
    { name: "VS Code", icon: CodeXml },
    { name: "Cursor", icon: Box },
    { name: "Antigravity", icon: AppWindow },
    { name: "Zed", icon: Feather },
  ]
  return <>
    <section aria-label="Editors">
      <div className={consoleStyles.connectionLabel}><h3>Editors</h3><span>SSH setup preview</span></div>
      <div className={consoleStyles.editors}>{editors.map(({ name, icon: Icon }) =>
        <div className={consoleStyles.editor} key={name}><i><Icon size={18} /></i><span>{name}</span></div>
      )}</div>
    </section>
    <section aria-label="Mobile">
      <div className={consoleStyles.connectionLabel}><h3>Mobile</h3></div>
      <div className={consoleStyles.phoneCard}>
        <div className={consoleStyles.qr}><PreviewQR /></div>
        <div><strong>Scan with your phone</strong>
          <p>This example QR code displays a preview message.</p>
          <span>Connection details and tokens</span>
        </div>
      </div>
    </section>
  </>
}

export function WorkspaceActions({
  machine: m,
  actions,
  compact = false,
  surface,
  capture,
  captureBlock,
}: {
  machine: Machine
  actions: Actions
  compact?: boolean
  surface?: "row" | "detail"
  capture?: () => void
  captureBlock?: string
}) {
  const quickTerminal = useTerminalLaunch(m, actions)
  const [connect, setConnect] = useState(fixtureScene === "30" && surface === "detail")
  const connectButton = useRef<HTMLButtonElement>(null)
  const changeConnect = (open: boolean) => {
    setConnect(open)
    if (!open) requestAnimationFrame(() => connectButton.current?.focus())
  }
  const power = machinePowerState(m)
  const deep = bundleBound(m)
  const running = power === "running",
    paused = !deep && (power === "hibernated" || m.observed_state === "paused" && m.availability !== "unknown"),
    cold = ["created", "stopped"].includes(power)
  const canCapture = running || power === "standby"
  const nativeActions = [
    { key: "lifecycle.standby", name: "Standby", icon: Moon, available: running, act: () => powerDialog(m, actions, "standby") },
    { key: "state.hibernate", name: "Hibernate", icon: Moon, available: canCapture, act: () => powerDialog(m, actions, "hibernate") },
    { key: "state.independent_fork", name: "Independent Fork", icon: GitFork, available: canCapture, act: () => copyDialog(m, actions, true, true) },
    { key: "state.fork_batch", name: "Fork batch", icon: GitFork, available: canCapture, act: () => forkBatchDialog(m, actions) },
  ]
  return (
    <div className={consoleStyles.workspaceActions} data-surface={surface}>
      <Button className={consoleStyles.openButton} ref={connectButton} variant={compact && !surface ? "ghost" : "default"} size={compact && !surface ? "icon-sm" : "sm"}
        aria-label={"Open " + m.name} onClick={() => changeConnect(true)}>
        <ExternalLink />{(!compact || surface) && "Open"}
      </Button>
      {surface === "detail" && <div className={consoleStyles.powerActions}>
        <Button variant="outline" size="sm" disabled={Boolean(captureBlock)} title={captureBlock} onClick={capture}>Snapshot</Button>
        {cold && <Button variant="outline" size="sm" onClick={() => powerDialog(m, actions, "start")}>Start</Button>}
        {running && deep && nativeActions.slice(0, 2).map(({ key, name, available, act }) => {
          const reason = fullStateBlock(m, key)
          return <Button key={key} variant="outline" size="sm" disabled={!available || Boolean(reason)} title={reason} onClick={act}>{name}</Button>
        })}
        {running && !deep && <Button variant="outline" size="sm" disabled={m.capabilities?.pause?.supported === false}
          onClick={() => powerDialog(m, actions, "pause")}>Hibernate</Button>}
        {running && <Button variant="outline" size="sm" disabled={deep && Boolean(fullStateBlock(m, "lifecycle.stop"))}
          onClick={() => powerDialog(m, actions, "stop")}>Stop</Button>}
        {(power === "standby" || power === "hibernated") && <Button variant="outline" size="sm"
          disabled={deep && power === "standby" && Boolean(fullStateBlock(m, "lifecycle.standby_wake"))}
          onClick={() => powerDialog(m, actions, paused ? "resume" : "wake")}>Wake</Button>}
      </div>}
      {surface === "detail" && <div className={consoleStyles.mobileConsoles}>
        <Button variant="outline" onClick={() => void quickTerminal.openTerminal()} disabled={quickTerminal.opening}>
          <SquareTerminal />{quickTerminal.opening ? "Opening…" : "Web terminal"}
        </Button>
        {m.desktop_profile === "software-vnc-v1" && <Button variant="outline" asChild><a href={desktopHref(m.machine_id)}><Monitor />Desktop</a></Button>}
        <Callout error={quickTerminal.error} />
      </div>}
      <ConnectDialog machine={m} actions={actions} open={connect} onOpenChange={changeConnect} />
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button className={surface === "detail" ? consoleStyles.extraActions : undefined} size="icon-sm" variant={compact ? "ghost" : "outline"} aria-label={"Actions for " + m.name}>
            <Ellipsis />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="min-w-48">
          <DropdownMenuItem onSelect={() => changeConnect(true)}>
            <Cable />
            Connection options
          </DropdownMenuItem>
          {(!deep || m.capabilities?.live_fork) && <DropdownMenuItem
            disabled={!running || (deep ? m.capabilities?.live_fork?.supported !== true : m.capabilities?.live_fork?.supported === false)}
            onSelect={() => copyDialog(m, actions, true)}
          >
            <GitFork />
            Live Fork
          </DropdownMenuItem>}
          <DropdownMenuItem
            disabled={deep || m.capabilities?.independent_clone?.supported === false}
            onSelect={() => copyDialog(m, actions, false)}
          >
            <Layers2 />
            Managed clean clone
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {deep && nativeActions.map(({ key, name, icon: Icon, available, act }) => {
            const reason = fullStateBlock(m, key)
            return <DropdownMenuItem key={key} disabled={!available || Boolean(reason)} title={reason} onSelect={act}>
              <Icon />{name}
            </DropdownMenuItem>
          })}
          {!deep && running && (
            <DropdownMenuItem disabled={m.capabilities?.pause?.supported === false} onSelect={() => powerDialog(m, actions, "pause")}>
              <Moon />
              Hibernate
            </DropdownMenuItem>
          )}
          {paused && (
            <DropdownMenuItem onSelect={() => powerDialog(m, actions, "resume")}>
              <Play />
              Resume
            </DropdownMenuItem>
          )}
          {cold && (
            <DropdownMenuItem onSelect={() => powerDialog(m, actions, "start")}>
              <Play />
              Cold start
            </DropdownMenuItem>
          )}
          {(power === "standby" || power === "hibernated") && <DropdownMenuItem
            disabled={deep && power === "standby" && Boolean(fullStateBlock(m, "lifecycle.standby_wake"))}
            title={deep && power === "standby" ? fullStateBlock(m, "lifecycle.standby_wake") : undefined}
            onSelect={() => powerDialog(m, actions, "wake")}>
            <Zap />Wake
          </DropdownMenuItem>}
          {running && (
            <DropdownMenuItem disabled={deep && Boolean(fullStateBlock(m, "lifecycle.stop"))} title={deep ? fullStateBlock(m, "lifecycle.stop") : undefined} onSelect={() => powerDialog(m, actions, "stop")}>
              <Power />
              Cold stop
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            className="text-destructive-foreground"
            disabled={deep && (m.boot_mode !== "system" || power !== "stopped")}
            title={deep && m.boot_mode === "system" && power !== "stopped" ? "Stop the System machine before deleting it" : undefined}
            onSelect={() => powerDialog(m, actions, "delete")}
          >
            <Trash2 />
            Delete machine
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
