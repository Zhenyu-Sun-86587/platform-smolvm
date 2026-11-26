import { useEffect, useRef, useState } from "react"
import { FolderClosed, Globe2, SquareTerminal } from "lucide-react"
import * as Dialog from "@radix-ui/react-dialog"
import { Skeleton } from "../../ui"
import styles from "./console.module.css"

const apps = [
  { name: "Browser", icon: Globe2 },
  { name: "Terminal", icon: SquareTerminal },
  { name: "Files", icon: FolderClosed },
]

export function DesktopLauncher({ open, onOpenChange, scene }: {
  open: boolean; onOpenChange: (open: boolean) => void; scene: string
}) {
  const [search, setSearch] = useState(scene === "40" ? "no-match" : "")
  const [highlight, setHighlight] = useState(0)
  const [starting, setStarting] = useState("")
  const lastFocus = useRef<HTMLElement | null>(null)
  const loading = scene === "34"
  const matches = loading ? [] : apps.filter(app => app.name.toLowerCase().includes(search.toLowerCase().trim()))

  useEffect(() => {
    if (!starting) return
    const timer = setTimeout(() => {
      setStarting("")
      if (scene !== "launcher-failed") onOpenChange(false)
    }, 850)
    return () => clearTimeout(timer)
  }, [starting, scene, onOpenChange])

  return <Dialog.Root open={open} onOpenChange={onOpenChange}>
    <Dialog.Portal><Dialog.Overlay className={styles.launcherOverlay} />
    <Dialog.Content className={styles.launcherModal} aria-describedby={undefined}
      onOpenAutoFocus={() => { lastFocus.current = document.activeElement as HTMLElement | null }}
      onCloseAutoFocus={event => { event.preventDefault(); lastFocus.current?.focus() }} onKeyDown={event => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "b") {
        event.preventDefault()
        event.stopPropagation()
        onOpenChange(false)
      }
      if (!matches.length || starting || event.target instanceof HTMLButtonElement && event.key === "Enter") return
      if (event.key === "ArrowRight" || event.key === "ArrowDown") setHighlight((highlight + 1) % matches.length)
      else if (event.key === "ArrowLeft" || event.key === "ArrowUp") setHighlight((highlight + matches.length - 1) % matches.length)
      else if (event.key === "Enter") setStarting(matches[highlight % matches.length].name)
      else return
      event.preventDefault()
    }}>
      <Dialog.Title className="sr-only">Launch an app</Dialog.Title>
      <input aria-label="Find an app" placeholder="Launch an app…" value={search}
        onChange={event => { setSearch(event.target.value); setHighlight(0) }} />
      {loading ? <div className={styles.appGrid} role="status" aria-label="Loading apps">
        {[0, 1, 2, 3].map(index => <div className={styles.appPlaceholder} key={index}><Skeleton /><Skeleton /></div>)}
      </div> : matches.length ? <div className={styles.appGrid}>{matches.map(({ name, icon: Icon }, index) =>
        <button type="button" key={name} className={styles.appTile} data-highlighted={index === highlight} disabled={Boolean(starting)}
          onMouseEnter={() => setHighlight(index)} onFocus={() => setHighlight(index)} onClick={() => setStarting(name)}>
          <span className={styles.appIcon}><Icon size={20}/></span><span>{starting === name ? "Starting…" : name}</span>
        </button>)}</div> : <p role="status">No matching apps.</p>}
      {scene === "launcher-failed" && !starting && <p role="alert">The guest did not start the requested app.</p>}
    </Dialog.Content></Dialog.Portal>
  </Dialog.Root>
}
