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
import { Callout, segment, str, label, useRemote } from "./common"
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
        : kind === "stop"
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
export function copyDialog(m: Machine, actions: Actions, live: boolean) {
  actions.dialog({
    title: live ? "Live Fork" : "Independent Clone",
    submit: live ? "Create fork" : "Create clone",
    description: live
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
        },
        { revision: m.revision },
      ),
  })
}
function SSHInfo({ machine }: { machine: Machine }) {
  const { data, error } = useRemote<Page<Service>>("machines/" + segment(machine.machine_id) + "/services", 0)
  const [copied, setCopied] = useState(false),
    [copyError, setError] = useState<unknown>()
  const service = data?.items.find((item) => item.protocol === "ssh" && item.enabled !== false)
  const cmd = "sandboxctl ssh-config " + machine.machine_id
  return (
    <div className="ssh-info">
      <Callout error={error || copyError} />
      {!data && !error ? (
        "Checking published services…"
      ) : service ? (
        <>
          <p>Generate an OpenSSH configuration with this machine’s verified host key.</p>
          <div className="copy-line">
            <code>{cmd}</code>
            <Button
              size="icon-sm"
              variant="outline"
              aria-label="Copy SSH command"
              onClick={() =>
                navigator.clipboard
                  .writeText(cmd)
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
  const running = m.observed_state === "running",
    paused = m.observed_state === "paused",
    cold = ["created", "stopped"].includes(m.observed_state)
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
        asChild
      >
        <a href={base + "/terminal"}>
          <SquareTerminal />
          {!compact && "Terminal"}
        </a>
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
          <DropdownMenuItem
            disabled={!running || m.capabilities?.live_fork?.supported === false}
            onSelect={() => copyDialog(m, actions, true)}
          >
            <GitFork />
            Live Fork
          </DropdownMenuItem>
          <DropdownMenuItem
            disabled={m.capabilities?.independent_clone?.supported === false}
            onSelect={() => copyDialog(m, actions, false)}
          >
            <Layers2 />
            Independent Clone
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {running && (
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
          <DropdownMenuItem onSelect={() => powerDialog(m, actions, "wake")}>
            <Zap />
            Wake
          </DropdownMenuItem>
          {running && (
            <DropdownMenuItem onSelect={() => powerDialog(m, actions, "stop")}>
              <Power />
              Cold stop
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            className="text-destructive-foreground"
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
