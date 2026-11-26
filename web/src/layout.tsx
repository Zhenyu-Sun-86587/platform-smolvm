import * as Dialog from "@radix-ui/react-dialog"
import { useEffect, useRef, useState } from "react"
import type { ReactNode } from "react"
import {
  Activity, Bot, Box, ChevronRight, ChevronsLeft, Code2, Globe2, HardDrive, Home,
  KeyRound, Layers3, Link2, ListChecks, LockKeyhole, LogOut, Menu, Moon, PanelsTopLeft,
  Plus, Search, Server, Settings2, Sun, X,
} from "lucide-react"
import type { Machine, Snapshot } from "../../sdk/typescript/src/index"
import { fixtureScene, useCollection } from "./api"
import { CommandPalette } from "./features/shell/CommandPalette"
import type { CommandGroup } from "./features/shell/CommandPalette"
import { machineHref } from "./routes"
import { IconButton, useMobile } from "./ui"

const workspace = [
  { page: "dashboard", name: "Dashboard", icon: Home },
  { page: "machines", name: "Machines", icon: Server, keywords: "vm workspaces environments" },
  { page: "snapshots", name: "Snapshots", icon: Layers3, keywords: "capture restore" },
  { page: "volumes", name: "Volumes", icon: HardDrive, keywords: "disks storage" },
  { page: "tasks", name: "Tasks", icon: ListChecks, keywords: "automations jobs" },
  { page: "agents", name: "Agents", icon: Bot, fixtureOnly: true },
]
const access = [
  { page: "networking", name: "Networking", icon: Globe2, keywords: "services domains ports" },
  { page: "integrations", name: "Integrations", icon: Link2, fixtureOnly: true },
  { page: "environment", name: "Environment variables", icon: LockKeyhole, fixtureOnly: true },
  { page: "api-keys", name: "API keys", icon: KeyRound, keywords: "tokens credentials" },
  { page: "domains", name: "Domains", icon: Globe2, fixtureOnly: true },
]
const platform = [
  { page: "settings", name: "Settings", icon: Settings2 },
  { page: "operations", name: "Operations", icon: Activity, keywords: "operation progress" },
]
type NavigationItem = (typeof workspace | typeof access | typeof platform)[number]
const groups = [
  { label: "Workspace", items: workspace },
  { label: "Access", items: access },
  { label: "Platform", items: platform },
]
const additional = [
  { page: "diagnostics", name: "Runtime diagnostics", icon: Activity },
  { page: "templates", name: "Templates", icon: PanelsTopLeft },
  { page: "backups", name: "Backups", icon: HardDrive },
]
const all = [...workspace, ...access, ...platform, ...additional]
const initiallyOpen = ["14", "20", "35"].includes(fixtureScene ?? "")

function stored(key: string) {
  try { return localStorage.getItem(key) } catch { return undefined }
}
function save(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch {}
}

export function Brand({ collapsed = false }: { collapsed?: boolean }) {
  return <a className="brand" href="#/dashboard" aria-label="Sandbox dashboard">
    <span className="brand-mark"><Box size={18} strokeWidth={1.75} /></span>
    {!collapsed && <span>Sandbox</span>}
  </a>
}
export function SourceLinks() {
  return <div className="source-links">
    <Code2 size={14} aria-hidden="true" /><a href="/assets/console-source.tar.gz" download>Source</a>
    <span aria-hidden="true">·</span><a href="/assets/console-notices.txt" target="_blank" rel="noreferrer">Licenses</a>
  </div>
}

function allowsShortcuts(target: EventTarget | null) {
  return !(target instanceof Element && target.closest(
    "input, textarea, select, [contenteditable]:not([contenteditable=false]), .xterm, [data-app-shortcuts=off]",
  ))
}

export function Shell({ page, machines, runtime, ready, username, signOut, revision, children }: {
  page: string
  machines?: Machine[]
  runtime?: string
  ready?: boolean
  username?: string
  signOut: () => void
  revision: number
  children: (commands: { onSearch: () => void }) => ReactNode
}) {
  const mobile = useMobile()
  const [menuOpen, setMenuOpen] = useState(fixtureScene === "42")
  const [collapsed, setCollapsed] = useState(() => !fixtureScene && stored("sandbox.navigation") === "collapsed")
  const [searchOpen, setSearchOpen] = useState(initiallyOpen)
  const [fixturePalette, setFixturePalette] = useState(initiallyOpen)
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"))
  const navigationTrigger = useRef<HTMLButtonElement>(null)
  const finder = useRef<HTMLButtonElement>(null)
  const commandTrigger = useRef<HTMLElement | null>(null)
  const snapshots = useCollection<Snapshot>(searchOpen ? "snapshots" : "", revision)
  const selected = all.find(item => item.page === page)
  const title = selected?.name ?? "Workspace"
  const navActive = ["diagnostics", "templates", "backups"].includes(page) ? "settings" : page
  const apple = /Mac|iPhone|iPad/.test(navigator.platform)
  const shortcut = apple ? "⌘ B" : "Ctrl B"

  const openSearch = () => {
    commandTrigger.current = document.activeElement as HTMLElement | null
    setFixturePalette(false)
    setSearchOpen(true)
  }
  const restoreSearchFocus = () => {
    const target = commandTrigger.current
    if (target?.isConnected && target !== document.body) {
      target.focus({ preventScroll: true })
    } else {
      (mobile ? navigationTrigger.current : finder.current)?.focus({ preventScroll: true })
    }
  }
  useEffect(() => {
    const key = (event: globalThis.KeyboardEvent) => {
      if (event.defaultPrevented || event.isComposing || event.repeat || event.altKey ||
        !(event.metaKey || event.ctrlKey) || !["b", "k"].includes(event.key.toLowerCase())) return
      if (!searchOpen && !allowsShortcuts(event.target)) return
      event.preventDefault()
      if (searchOpen) setSearchOpen(false)
      else openSearch()
    }
    const close = () => { setMenuOpen(false); setSearchOpen(false) }
    addEventListener("keydown", key)
    addEventListener("hashchange", close)
    return () => {
      removeEventListener("keydown", key)
      removeEventListener("hashchange", close)
    }
  }, [searchOpen])

  const visible = (item: NavigationItem) => Boolean(fixtureScene) || !("fixtureOnly" in item && item.fixtureOnly)
  const commands: CommandGroup[] = [
    { name: "Go to", commands: all.filter(visible).map(({ page, name, icon: Icon, ...item }) => ({
      id: page, name, href: "#/" + page, keywords: "keywords" in item && typeof item.keywords === "string" ? item.keywords : undefined,
      icon: <Icon size={17} aria-hidden="true" />,
    })) },
    { name: "Create", commands: [{ id: "new-machine", name: "New machine", href: "#/machines?action=new", icon: <Plus size={17} /> },
      { id: "new-key", name: "New API key", href: "#/api-keys?action=new", icon: <Plus size={17} /> }] },
    { name: "Machines", searchOnly: true, commands: (machines ?? []).map(machine => ({
      id: "machine-" + machine.machine_id, name: machine.name, href: machineHref(machine.machine_id),
      keywords: machine.machine_id, icon: <Box size={17} aria-hidden="true" />,
    })) },
    { name: "Saved states", searchOnly: true, commands: (snapshots.items ?? []).map(snapshot => ({
      id: "snapshot-" + snapshot.snapshot_id, name: snapshot.label ?? snapshot.snapshot_id,
      href: snapshot.source_machine_id ? machineHref(snapshot.source_machine_id, "checkpoints") : "#/snapshots",
      keywords: snapshot.snapshot_id, icon: <Layers3 size={17} aria-hidden="true" />,
    })) },
  ]

  const navigation = (compact: boolean, drawer: boolean) => <>
    <div className="brand-line">
      <Brand collapsed={compact} />
      {drawer
        ? <Dialog.Close asChild><IconButton aria-label="Close navigation"><X size={17} /></IconButton></Dialog.Close>
        : <IconButton className="navigation-collapse" aria-label={compact ? "Expand sidebar" : "Collapse sidebar"}
          onClick={() => { setCollapsed(!collapsed); save("sandbox.navigation", collapsed ? "expanded" : "collapsed") }}>
          {compact ? <ChevronRight size={15} /> : <ChevronsLeft size={16} />}
        </IconButton>}
    </div>
    <button className="finder" onClick={openSearch} aria-label="Open command palette" ref={drawer ? undefined : finder} title={shortcut}>
      <Search size={16} aria-hidden="true" />{!compact && <><span>Search workspace</span>{!drawer && <kbd>{shortcut}</kbd>}</>}
    </button>
    <div className="shell-links">
      {groups.map(({ label, items }) => <nav aria-label={label} key={label}>
        {!compact && <div className="nav-label" aria-hidden="true">{label}</div>}
        {items.filter(visible).map(({ page: itemPage, name, icon: Icon }) =>
          <a key={itemPage} href={"#/" + itemPage} aria-current={navActive === itemPage ? "page" : undefined}
            aria-label={compact ? name : undefined} title={compact ? name : undefined}
            onClick={drawer ? () => setMenuOpen(false) : undefined}>
            <Icon size={17} aria-hidden="true" />{!compact && name}
          </a>)}
      </nav>)}
    </div>
    <footer className="sidebar-footer">
      <a href="#/diagnostics" className="sidebar-diagnostics" title="Runtime diagnostics"
        aria-label={compact ? "Runtime diagnostics" : undefined} onClick={drawer ? () => setMenuOpen(false) : undefined}>
        <Activity size={16} aria-hidden="true" />{!compact && <><span>Runtime diagnostics</span><ChevronRight size={14} /></>}
      </a>
      {!compact && <SourceLinks />}
      <div className="account">
        {!compact && <><span className="avatar" aria-hidden="true">{username?.[0]?.toUpperCase() ?? "S"}</span>
          <div>{username ?? "Local account"}<small>{fixtureScene ? "Development fixture" : "Self-hosted workspace"}</small></div></>}
        <IconButton title="Sign out" aria-label="Sign out" onClick={signOut}><LogOut size={17} /></IconButton>
      </div>
    </footer>
  </>

  return <div className="shell" data-sidebar-collapsed={collapsed ? "" : undefined}>
    {!mobile && <aside className="sidebar" aria-label="Sidebar">{navigation(collapsed, false)}</aside>}
    <div className="main-column">
      <header className="topbar">
        {mobile
          ? <IconButton ref={navigationTrigger} aria-label="Open navigation" aria-expanded={menuOpen} onClick={() => setMenuOpen(true)}><Menu size={19} /></IconButton>
          : <><a className="workspace-crumb" href="#/dashboard">Workspace</a><ChevronRight size={13} aria-hidden="true" /></>}
        <span className="topbar-page">{title}</span>
        <div className="top-tools">
          {runtime && !mobile && <a href="#/diagnostics" className="runtime" title={ready ? "Runtime ready" : "Runtime readiness unknown or unavailable"}>
            <span data-ready={Boolean(ready)} aria-hidden="true" /><span>smolvm {runtime}</span>
          </a>}
          {mobile && <IconButton aria-label="Open command palette" onClick={openSearch}><Search size={18} /></IconButton>}
          <IconButton aria-label={dark ? "Use light theme" : "Use dark theme"} onClick={() => {
            document.documentElement.classList.toggle("dark", !dark)
            save("sandbox.theme", dark ? "light" : "dark")
            setDark(!dark)
          }}>{dark ? <Sun size={17} /> : <Moon size={17} />}</IconButton>
        </div>
      </header>
      <main className="workspace" id="workspace">{children({ onSearch: openSearch })}</main>
    </div>
    {mobile && <Dialog.Root open={menuOpen} onOpenChange={setMenuOpen}>
      <Dialog.Portal>
        <Dialog.Overlay className="console-overlay navigation-overlay" />
        <Dialog.Content className="sidebar sidebar-drawer" aria-describedby={undefined}
          onCloseAutoFocus={event => { event.preventDefault(); navigationTrigger.current?.focus({ preventScroll: true }) }}>
          <Dialog.Title className="sr-only">Workspace navigation</Dialog.Title>
          {navigation(false, true)}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>}
    {searchOpen && <CommandPalette groups={commands} account={username} onClose={() => setSearchOpen(false)}
      restoreFocus={restoreSearchFocus} initialQuery={fixturePalette && fixtureScene === "20" ? "snapshot" : ""}
      initialIndex={fixturePalette && fixtureScene === "35" ? 1 : 0} />}
  </div>
}
