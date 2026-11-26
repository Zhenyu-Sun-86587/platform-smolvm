import * as Dialog from "@radix-ui/react-dialog"
import { Search, X } from "lucide-react"
import { useId, useMemo, useRef, useState } from "react"
import type { KeyboardEvent, ReactNode } from "react"
import { fixtureScene } from "../../api"
import { IconButton } from "../../ui"

export type Command = { id: string; name: string; icon: ReactNode; keywords?: string; href: string }
export type CommandGroup = { name: string; commands: Command[]; searchOnly?: boolean }
type Match = { command: Command; score: number; positions: number[] }

const maxPerGroup = 6

function matchText(query: string, text: string, highlight: boolean) {
  const haystack = text.toLowerCase()
  const positions: number[] = []
  let previous = -1
  let score = 0
  for (const char of query) {
    const index = haystack.indexOf(char, previous + 1)
    if (index < 0) return
    if (index === 0 || /[\s_.-]/.test(haystack[index - 1])) score += 5
    if (index === previous + 1) score += 4
    score -= (index - previous) * 0.15
    previous = index
    positions.push(index)
  }
  if (haystack.startsWith(query)) score += 10
  if (haystack === query) score += 20
  return { score, positions: highlight ? positions : [] }
}

function matchCommand(query: string, command: Command): Match[] {
  const name = matchText(query, command.name, true)
  const keyword = command.keywords && matchText(query, command.keywords, false)
  const match = name ?? (keyword ? { ...keyword, score: keyword.score - 6 } : undefined)
  return match ? [{ command, ...match }] : []
}

const unmatched = (command: Command): Match => ({ command, score: 0, positions: [] })
const byScore = (a: Match, b: Match) => b.score - a.score || a.command.name.localeCompare(b.command.name)

function readRecent(key: string) {
  if (fixtureScene) return []
  try {
    const value: unknown = JSON.parse(localStorage.getItem(key) ?? "[]")
    if (Array.isArray(value)) return value.filter((id): id is string => typeof id === "string").slice(0, maxPerGroup)
  } catch {}
  return []
}

export function CommandPalette({ groups, onClose, restoreFocus, account, initialQuery = "", initialIndex = 0 }: {
  groups: CommandGroup[]
  account?: string
  onClose: () => void
  restoreFocus: () => void
  initialQuery?: string
  initialIndex?: number
}) {
  const listId = useId()
  const input = useRef<HTMLInputElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const storageKey = "sandbox.palette." + (account ?? "local")
  const [recent] = useState(() => readRecent(storageKey))
  const [query, setQuery] = useState(initialQuery)
  const [active, setActive] = useState(initialIndex)

  const results = useMemo(() => {
    const term = query.toLowerCase().trim()
    if (term) return groups.map(group => ({
      name: group.name,
      matches: group.commands.flatMap(command => matchCommand(term, command)).sort(byScore),
    })).filter(group => group.matches.length).sort((a, b) => byScore(a.matches[0], b.matches[0]))

    const commands = new Map(groups.flatMap(group => group.commands).map(command => [command.id, command]))
    const prior = recent.flatMap(id => {
      const command = commands.get(id)
      return command ? [unmatched(command)] : []
    })
    return [
      ...(prior.length ? [{ name: "Recent", matches: prior }] : []),
      ...groups.filter(group => !group.searchOnly && group.commands.length).map(group => ({
        name: group.name, matches: group.commands.map(unmatched),
      })),
    ]
  }, [groups, query, recent])
  const options = results.flatMap(group => group.matches.slice(0, maxPerGroup))
  const selected = options.length ? Math.min(active, options.length - 1) : -1
  const optionId = (position: number) => listId + "-" + position

  const run = (match: Match) => {
    const { id, href } = match.command
    if (!fixtureScene) {
      const row = [id, ...recent.filter(value => value !== id)].slice(0, maxPerGroup)
      try { localStorage.setItem(storageKey, JSON.stringify(row)) } catch {}
    }
    onClose()
    location.hash = href
  }
  const move = (position: number) => {
    setActive(position)
    const element = document.getElementById(optionId(position))
    if (element && list.current?.contains(element)) element.scrollIntoView({ block: "nearest" })
  }
  const navigate = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing || !options.length) return
    const length = options.length
    switch (event.key) {
      case "ArrowDown":
        move((selected + 1) % length)
        break
      case "ArrowUp":
        move((selected - 1 + length) % length)
        break
      case "Home":
        move(0)
        break
      case "End":
        move(length - 1)
        break
      case "Enter":
        run(options[selected])
        break
      default:
        return
    }
    event.preventDefault()
  }

  let row = -1
  return <Dialog.Root open onOpenChange={open => { if (!open) onClose() }}>
    <Dialog.Portal>
      <Dialog.Overlay className="console-overlay palette-overlay" />
      <Dialog.Content className="console-palette" aria-describedby={undefined}
        onOpenAutoFocus={event => { event.preventDefault(); input.current?.focus({ preventScroll: true }) }}
        onCloseAutoFocus={event => { event.preventDefault(); restoreFocus() }}>
        <Dialog.Title className="sr-only">Command palette</Dialog.Title>
        <div className="palette-query">
          <Search size={17} aria-hidden="true" />
          <input ref={input} role="combobox" aria-label="Search workspace" aria-autocomplete="list" aria-expanded="true"
            aria-controls={listId} aria-activedescendant={selected >= 0 ? optionId(selected) : undefined}
            placeholder="Search machines, snapshots, settings, actions…" autoComplete="off" spellCheck={false} value={query}
            onChange={event => { setQuery(event.target.value); setActive(0); if (list.current) list.current.scrollTop = 0 }}
            onKeyDown={navigate} />
          <Dialog.Close asChild><IconButton className="palette-dismiss" aria-label="Close command palette" data-dialog-close><kbd aria-hidden="true">esc</kbd><X size={16} aria-hidden="true" /></IconButton></Dialog.Close>
        </div>
        <div id={listId} role="listbox" aria-label="Commands" className="palette-results" ref={list}>
          {results.map(group => <div key={group.name} role="group" aria-label={group.name}>
            <div className="palette-group" aria-hidden="true">{group.name}</div>
            {group.matches.slice(0, maxPerGroup).map(match => {
              const index = ++row
              const { command, positions } = match
              return <div className="palette-option" role="option" aria-selected={selected === index} id={optionId(index)}
                key={command.id} onPointerMove={event => { if (event.pointerType === "mouse") setActive(index) }}
                onMouseDown={event => event.preventDefault()} onClick={() => run(match)}>
                {command.icon}
                <span>{positions.length ? command.name.split("").map((char, index) =>
                  positions.includes(index) ? <mark key={index}>{char}</mark> : char) : command.name}</span>
              </div>
            })}
            {group.matches.length > maxPerGroup && <p className="palette-more">+{group.matches.length - maxPerGroup} more, keep typing to narrow</p>}
          </div>)}
        </div>
        {results.length === 0 && <p className="palette-no-results" role="status">No results for “{query.trim()}”.</p>}
        <footer className="palette-footer"><span><kbd>↑↓</kbd> Navigate</span><span><kbd>↵</kbd> Open</span><span><kbd>esc</kbd> Close</span></footer>
      </Dialog.Content>
    </Dialog.Portal>
  </Dialog.Root>
}
