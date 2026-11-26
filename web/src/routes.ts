export const pages = [
  "dashboard", "machines", "snapshots", "volumes", "tasks", "networking",
  "api-keys", "settings", "operations", "diagnostics", "templates", "backups",
  "agents", "integrations", "domains", "environment",
] as const

export type Page = typeof pages[number]
export type Route =
  | { page: Page; id?: string; action?: "new" }
  | { page: "machine"; id: string; tab: string }
  | { page: "desktop"; id: string }
  | { page: "terminal"; id: string; execId?: string }

export const machineHref = (id: string, tab?: string) =>
  "#/machines/" + encodeURIComponent(id) + (tab ? "/" + encodeURIComponent(tab) : "")
export const terminalHref = (id: string, execId?: string) =>
  "#/terminal/" + encodeURIComponent(id) + (execId ? "/" + encodeURIComponent(execId) : "")
export const desktopHref = (id: string) => "#/desktop/" + encodeURIComponent(id)

export function readRoute(hash: string): Route {
  const [head, rawId, rawTab] = hash.replace(/^#\/?/, "").split("?")[0].split("/")
  try {
    const id = rawId ? decodeURIComponent(rawId) : undefined
    const tab = rawTab ? decodeURIComponent(rawTab) : undefined
    if (head === "machines" && id) return { page: "machine", id, tab: tab ?? "overview" }
    if (head === "terminal" && id) return { page: "terminal", id, execId: tab }
    if (head === "desktop" && id) return { page: "desktop", id }
    const action = new URLSearchParams(hash.split("?")[1]).get("action") === "new" ? "new" : undefined
    const page = pages.find(page => page === head)
    if (page) return { page, id, action }
  } catch {
    return { page: "machines" }
  }
  return { page: head ? "machines" : "dashboard" }
}
