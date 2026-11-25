import { useEffect, useState, type ReactNode } from "react"
import {
  Activity,
  Archive,
  Box,
  ChevronRight,
  Code2,
  Globe2,
  Layers3,
  ListChecks,
  LogOut,
  Menu,
  Moon,
  Search,
  Server,
  Settings2,
  Sun,
  X,
} from "lucide-react"
import { SidebarMenu, SidebarMenuItem } from "./daytona/sidebar"
import { Button } from "./daytona/button"
import { Dialog, DialogContent, DialogTitle } from "./daytona/dialog"
import type { Machine } from "../../sdk/typescript/src/index"
import { segment } from "./api"

const workspace = [
  { page: "machines", name: "Machines", icon: Server },
  { page: "snapshots", name: "Snapshots", icon: Layers3 },
  { page: "tasks", name: "Tasks", icon: ListChecks },
  { page: "networking", name: "Networking", icon: Globe2 },
]
const system = [
  { page: "operations", name: "Operations", icon: Activity },
  { page: "settings", name: "Settings", icon: Settings2 },
]
const destinations = [...workspace, ...system]
export function Brand() {
  return (
    <a className="brand" href="#/machines">
      <span className="brand-mark">
        <Box size={21} strokeWidth={2} />
      </span>
      Sandbox
    </a>
  )
}
export function SourceLinks() {
  return (
    <div className="source-links">
      <Code2 size={14} />
      <a href="/assets/console-source.tar.gz" download>
        Source
      </a>
      <span>·</span>
      <a href="/assets/console-notices.txt" target="_blank" rel="noreferrer">
        Licenses
      </a>
    </div>
  )
}
export function Shell({
  page,
  machine,
  machines,
  runtime,
  ready,
  username,
  signOut,
  children,
}: {
  page: string
  machine?: Machine
  machines?: Machine[]
  runtime?: string
  ready?: boolean
  username?:string
  signOut: () => void
  children: ReactNode
}) {
  const active =
    page === "machine" ? "machines" : ["diagnostics", "templates"].includes(page) ? "settings" : page
  const name = destinations.find((item) => item.page === active)?.name ?? "Machines"
  const [mobile, setMobile] = useState(false)
  const [search, setSearch] = useState(false)
  const [query, setQuery] = useState("")
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"))
  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault()
        setQuery("")
        setSearch((open) => !open)
      }
    }
    const close = () => {
      setMobile(false)
      setSearch(false)
    }
    addEventListener("keydown", key)
    addEventListener("hashchange", close)
    return () => {
      removeEventListener("keydown", key)
      removeEventListener("hashchange", close)
    }
  }, [])
  const nav = (items: typeof workspace) =>
    items.map(({ page: p, name, icon: Icon }) => (
      <SidebarMenuItem key={p}><a href={"#/" + p} aria-current={active === p ? "page" : undefined}>
        <Icon size={16} />
        {name}
      </a></SidebarMenuItem>
    ))
  const needle = query.trim().toLowerCase()
  const results = [
    ...destinations.map((i) => ({ name: i.name, url: "#/" + i.page, Icon: i.icon })),
    ...(machines ?? []).map((m) => ({ name: m.name, url: "#/machines/" + segment(m.machine_id), Icon: Box })),
  ].filter((i) => i.name.toLowerCase().includes(needle))
  return (
    <div className="shell">
      {mobile && (
        <button className="backdrop" aria-label="Close navigation" onClick={() => setMobile(false)} />
      )}
      <aside className={"sidebar" + (mobile ? " opened" : "")}>
        <div className="brand-line">
          <Brand />
          <button className="mobile-only" aria-label="Close sidebar" onClick={() => setMobile(false)}>
            <X size={16} />
          </button>
        </div>
        <button
          className="finder"
          onClick={() => {
            setQuery("")
            setSearch(true)
          }}
        >
          <Search size={15} />
          Search workspace<kbd>⌘ K</kbd>
        </button>
        <nav aria-label="Workspace">
          <div className="nav-label">Workspace</div>
          <SidebarMenu>{nav(workspace)}</SidebarMenu>
        </nav>
        <nav aria-label="Platform">
          <div className="nav-label">Platform</div>
          <SidebarMenu>{nav(system)}</SidebarMenu>
        </nav>
        <footer className="sidebar-footer">
          <SourceLinks />
          <div className="account">
            <span className="avatar">{username?.[0]?.toUpperCase()??"•"}</span>
            <div>
              {username??"Local account"}<small>Self-hosted</small>
            </div>
            <Button variant="ghost" size="icon-sm" title="Sign out" aria-label="Sign out" onClick={signOut}>
              <LogOut />
            </Button>
          </div>
        </footer>
      </aside>
      <div className="main-column">
        <header className="topbar">
          <button className="mobile-only" aria-label="Open navigation" onClick={() => setMobile(true)}>
            <Menu size={17} />
          </button>
          <Archive size={15} />
          <span>Workspace</span>
          <ChevronRight size={13} />
          <a href={"#/" + active}>{name}</a>
          {machine && (
            <>
              <ChevronRight size={13} />
              <strong>{machine.name}</strong>
            </>
          )}
          <div className="top-tools">
            {runtime && (
              <a href="#/diagnostics" className="runtime" title={ready?"Runtime ready":"Runtime readiness unknown or unavailable"}>
                <span data-ready={Boolean(ready)} />
                smolvm {runtime}
              </a>
            )}
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={dark ? "Use light theme" : "Use dark theme"}
              onClick={() => {
                document.documentElement.classList.toggle("dark", !dark)
                localStorage.setItem("sandbox.theme", dark ? "light" : "dark")
                setDark(!dark)
              }}
            >
              {dark ? <Sun /> : <Moon />}
            </Button>
          </div>
        </header>
        <main className="workspace">{children}</main>
      </div>
      <Dialog open={search} onOpenChange={setSearch}>
        <DialogContent className="command" aria-describedby={undefined}>
          <DialogTitle>Search workspace</DialogTitle>
          <div className="search-box">
            <Search size={16} />
            <input
              aria-label="Search destinations and machines"
              placeholder="Find a page or machine…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              autoFocus
            />
          </div>
          <div className="search-results">
            {results.map(({ name, url, Icon }) => (
              <a key={url} href={url} onClick={() => setSearch(false)}>
                <Icon size={16} />
                {name}
                <ChevronRight size={14} />
              </a>
            ))}
            {!results.length && <p>No matches.</p>}
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
