import { useState } from "react"
import {
  Cable,
  Check,
  Copy,
  Ellipsis,
  GitFork,
  Layers2,
  Moon,
  Play,
  Power,
  SquareTerminal,
  Trash2,
  Zap,
} from "lucide-react"
import type { Machine, Service, Page } from "../../sdk/typescript/src/index"
import type { Actions } from "./main"
import { bundleBound, Callout, fullStateBlock, label, machinePowerState, segment, str, useRemote } from "./common"
import { Button } from "./daytona/button"
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "./daytona/dropdown-menu"

export function powerDialog(m: Machine, actions: Actions, kind: string) {
  const deleting = kind === "delete"
  const managed = Boolean(m.binding?.runtime_instance_id && m.binding?.runtime_bundle_id)
  actions.dialog({
    title: (kind === "stop" ? "Cold stop" : kind === "pause" ? "Hibernate" : label(kind)) + " machine",
    submit: label(kind),
    description:
      kind === "pause"
        ? "Save the complete running state, including memory, for a later resume."
        : kind === "stop"
          ? "This ends the running execution. The guest disk is retained."
          : deleting
            ? "Delete " + m.name + " and its runtime binding. Active dependencies can block deletion."
            : undefined,
    destructive: deleting || kind === "stop",
    fields:
      kind === "pause"
        ? [{ name: "allow_active_tasks", label: "Include active persistent tasks", type: "checkbox" }]
        : kind === "stop" && !managed
          ? [{ name: "cancel_tasks", label: "Cancel active persistent tasks first", type: "checkbox" }]
          : [],
    act: (f) =>
      actions.mutate(
        "machines/" + segment(m.machine_id) + (deleting ? "" : "/" + kind),
        {
          expected_generation: m.binding_generation,
          ...(f.has("allow_active_tasks") ? { allow_active_tasks: true } : {}),
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

function forkBatchDialog(m: Machine, actions: Actions) {
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
  const { data, error } = useRemote<Page<Service>>("machines/" + segment(machine.machine_id) + "/services", 0)
  const [copied, setCopied] = useState(false),
    [copyError, setError] = useState<unknown>()
  const service = data?.items.find((item) => item.protocol === "ssh" && item.enabled !== false)
  const cmd = "sandboxctl ssh-config " + machine.machine_id
  const hostKey = "sandboxctl ssh-host-key " + machine.machine_id
  return (
    <div className="ssh-info">
      <Callout error={error || copyError} />
      {!data && !error ? (
        "Checking published services…"
      ) : service ? (
        <>
          <p>Use the authenticated CLI for this platform. Add the verified key to <code>~/.ssh/sandbox_known_hosts</code> and the generated Host entry to your SSH configuration.</p>
          <pre className="ssh-commands">{hostKey+"\n"+cmd+"\nssh sandbox-"+machine.machine_id}</pre>
          <div className="copy-line">
            <span>Copy setup commands</span>
            <Button
              size="icon-sm"
              variant="outline"
              aria-label="Copy SSH command"
              onClick={() =>
                navigator.clipboard
                  .writeText(hostKey+"\n"+cmd+"\nssh sandbox-"+machine.machine_id)
                  .then(() => setCopied(true))
                  .catch(setError)
              }
            >
              {copied ? <Check /> : <Copy />}
            </Button>
          </div>
        </>
      ) : data ? (
        <p>Publish an SSH service for guest port 22 in Networking to connect as root.</p>
      ) : null}
    </div>
  )
}
export function WorkspaceActions({
  machine: m,
  actions,
  compact = false,
}: {
  machine: Machine
  actions: Actions
  compact?: boolean
}) {
  const base = "#/machines/" + segment(m.machine_id)
  const [opening,setOpening]=useState(false)
  async function openTerminal() {
    setOpening(true)
    try {
      const session=await actions.mutate<{exec_id:string}>("machines/"+segment(m.machine_id)+"/execs", {
        argv:["/bin/sh"], cwd:"/workspace", user:"root", tty:true,interactive:true,cols:100,rows:28
      }, {revision:m.revision})
      location.hash=base+"/terminal/"+segment(session.exec_id)
    } catch(error) {
      actions.dialog({title:"Open terminal",content:<Callout error={error}/>,submit:"View sessions",act:async()=>{location.hash=base+"/terminal"}})
    } finally {setOpening(false)}
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
  const ssh = () =>
    actions.dialog({
      title: "SSH connection",
      content: <SSHInfo machine={m} />,
      submit: "Open networking",
      act: async () => {
        location.hash = base + "/networking"
      },
    })
  return (
    <div className="machine-actions">
      {!compact && (
        <Button variant="outline" size="sm" onClick={ssh}>
          <Cable />
          SSH
        </Button>
      )}
      <Button
        variant={compact ? "ghost" : "default"}
        size={compact ? "icon-sm" : "sm"}
        aria-label={"Terminal for " + m.name}
        title="Open terminal"
        onClick={openTerminal}
        disabled={opening}
      >
        <SquareTerminal />
        {!compact && (opening?"Opening…":"Terminal")}
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button size="icon-sm" variant={compact ? "ghost" : "outline"} aria-label={"Actions for " + m.name}>
            <Ellipsis />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="min-w-48">
          <DropdownMenuItem onSelect={ssh}>
            <Cable />
            Connect via SSH
          </DropdownMenuItem>
          {!deep && <DropdownMenuItem
            disabled={!running || m.capabilities?.live_fork?.supported === false}
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
            <DropdownMenuItem onSelect={() => powerDialog(m, actions, "pause")}>
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
          <DropdownMenuItem disabled={deep && (power !== "standby" || Boolean(fullStateBlock(m, "lifecycle.standby_wake")))} onSelect={() => powerDialog(m, actions, "wake")}>
            <Zap />
            Wake
          </DropdownMenuItem>
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
