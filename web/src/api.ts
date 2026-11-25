import { Client, type Page } from "../../sdk/typescript/src/index"
import { useEffect, useState } from "react"

export const api = new Client(location.origin)
export const segment = encodeURIComponent
export async function collection<T>(path: string, signal: AbortSignal): Promise<T[]> {
  const result: T[] = []
  const seen = new Set<string>()
  let cursor: string | undefined
  do {
    const params = new URLSearchParams({ limit: "100" })
    if (cursor) params.set("cursor", cursor)
    const page = await api.get<Page<T>>(path + (path.includes("?") ? "&" : "?") + params, signal)
    result.push(...page.items)
    cursor = page.next_cursor
    if (cursor && seen.has(cursor)) throw new Error("The collection cursor did not advance.")
    if (cursor) seen.add(cursor)
  } while (cursor)
  return result
}

export function useCollection<T>(path: string, revision: number) {
  const [result, setResult] = useState<{ path: string; items?: T[]; error?: unknown }>()
  useEffect(() => {
    if (!path) return
    const abort = new AbortController()
    collection<T>(path, abort.signal).then(items => {
      if (!abort.signal.aborted) setResult({ path, items })
    }).catch(error => {
      if (!abort.signal.aborted) setResult({ path, error })
    })
    return () => abort.abort()
  }, [path, revision])
  return result?.path === path ? result : {}
}
