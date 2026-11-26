import { useRef, useState } from "react"
import {
  Cable,
  Check,
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
import type { Machine, Service } from "../../sdk/typescript/src/index"
import type { Actions } from "./main"
import { bundleBound, Callout, fullStateBlock, label, machinePowerState, segment, str } from "./common"
import { Button } from "./daytona/button"
import { Modal } from "./ui"
import { desktopHref, machineHref, terminalHref } from "./routes"
import { fixtureScene, useCollection } from "./api"
import consoleStyles from "./features/console/console.module.css"
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
    ],
    act: (f) =>
      actions.mutate(
        "machines/" + segment(m.machine_id) + (live ? "/forks" : "/clones"),
        {
          child_name: str(f, "child_name"),
          expected_generation: m.binding_generation,
          ...(live ? {} : { copy_policy: "managed_clean" }),
          ...(independent ? { require_independent: true } : {}),
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
      { name: "best_effort", label: "Create as many children as the Worker can admit", type: "checkbox" }],
    act: (f) => actions.mutate("machines/" + segment(m.machine_id) + "/fork-batches", {
      expected_generation: m.binding_generation,
      require_independent: true,
      ...(f.has("best_effort") ? { admission: "best_effort" } : {}),
      children: str(f, "children").split("\n").map(name => name.trim()).filter(Boolean).map(name => ({name})),
    }, { revision: m.revision }),
  })
}
function SSHInfo({ machine }: { machine: Machine }) {
  const { items, error } = useCollection<Service>("machines/" + segment(machine.machine_id) + "/services", machine.revision)
  const [copied, setCopied] = useState(false),
    [copyError, setError] = useState<unknown>()
  const service = items?.find((item) => item.protocol === "ssh" && item.enabled !== false)
  const cmd = "sandboxctl ssh-config " + machine.machine_id
  const hostKey = "sandboxctl ssh-host-key " + machine.machine_id
  const commands = hostKey + "\n" + cmd + "\nssh sandbox-" + machine.machine_id
  return <div className={consoleStyles.ssh}>
    <div className={consoleStyles.connectionLabel}><Cable size={15} /><h3>SSH with sandboxctl</h3></div>
    <Callout error={error || copyError} />
    {!items && !error ? <p role="status">Checking published services…</p> : service ? <>
      <p>Add the verified host key to <code>~/.ssh/sandbox_known_hosts</code> and the generated Host entry to your SSH configuration.</p>
      <div className={consoleStyles.commands}>
        <pre>{commands}</pre>
        <Button size="icon-sm" variant="outline" aria-label={copied ? "SSH commands copied" : "Copy SSH commands"}
          onClick={() => navigator.clipboard.writeText(commands).then(() => setCopied(true)).catch(setError)}>
          {copied ? <Check /> : <Copy />}
        </Button>
      </div>
    </> : items ? <p><a href={"#/machines/" + segment(machine.machine_id) + "/networking"}>Publish an SSH service</a> for guest port 22 to connect.</p> : null}
  </div>
}

export function ConnectDialog({ machine, actions, open, onOpenChange }: {
  machine: Machine; actions: Actions; open: boolean; onOpenChange: (open: boolean) => void
}) {
  const [opening, setOpening] = useState(false)
  const pending = useRef(false)
  const [error, setError] = useState<unknown>()
  const desktop = machine.desktop_profile === "software-vnc-v1"
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
      onOpenChange(false)
      location.hash = terminalHref(machine.machine_id, session.exec_id)
    } catch (error) {
      setError(error)
    } finally {
      pending.current = false
      setOpening(false)
    }
  }
  return <Modal open={open} onOpenChange={onOpenChange} busy={opening} size="md" title={`Connect to ${machine.name}`}
    description="Open a browser session or connect using your SSH client.">
    <div className={consoleStyles.connectBody}>
      <div className={consoleStyles.connectionLinks}>
        <Button className={consoleStyles.connectionLink} onClick={() => void openTerminal()} disabled={opening}>
          <SquareTerminal size={18}/><span><strong>{opening ? "Opening…" : "Web terminal"}</strong><small>Interactive guest shell</small></span>
        </Button>
        {desktop && <Button className={consoleStyles.connectionLink} variant="outline" asChild>
          <a href={desktopHref(machine.machine_id)} onClick={event => {
            if (opening) { event.preventDefault(); return }
            onOpenChange(false)
          }} aria-disabled={opening || undefined}>
            <Monitor size={18}/><span><strong>Desktop</strong><small>Guest display via VNC</small></span>
          </a>
        </Button>}
      </div>
      <Callout error={error} />
      <SSHInfo machine={machine} />
      <a className={consoleStyles.sessionsLink} href={machineHref(machine.machine_id, "terminal")} onClick={event => {
        if (opening) { event.preventDefault(); return }
        onOpenChange(false)
      }}><Waypoints size={15} /> Browse existing sessions</a>
    </div>
  </Modal>
}

export function WorkspaceActions({
  machine: m,
  actions,
  compact = false,
  surface,
}: {
  machine: Machine
  actions: Actions
  compact?: boolean
  surface?: "row" | "detail"
}) {
  const base = "#/machines/" + segment(m.machine_id)
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
      <Button ref={connectButton} variant={compact && !surface ? "ghost" : "default"} size={compact && !surface ? "icon-sm" : "sm"}
        aria-label={"Open " + m.name} onClick={() => changeConnect(true)}>
        <SquareTerminal />{(!compact || surface) && "Open"}
      </Button>
      {surface === "detail" && <Button variant="outline" size="sm" asChild><a href={base + "/checkpoints"}>Checkpoints</a></Button>}
      <ConnectDialog machine={m} actions={actions} open={connect} onOpenChange={changeConnect} />
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button size="icon-sm" variant={compact ? "ghost" : "outline"} aria-label={"Actions for " + m.name}>
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
