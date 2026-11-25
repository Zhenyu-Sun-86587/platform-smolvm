import { Client } from "../../sdk/typescript/src/index"
import { useEffect, useState } from "react"

export const api = new Client(location.origin)
export const segment = encodeURIComponent
export async function collection<T>(path: string, signal: AbortSignal): Promise<T[]> {
  const result: T[] = []
  for await (const item of api.pages<T>(path, 100, signal)) result.push(item)
  return result
}

export function useCollection<T>(path: string, revision: number): { items?: T[]; error?: unknown } {
  const [result, setResult] = useState<{ path: string; items?: T[]; error?: unknown }>()
  useEffect(() => {
    if (!path) return
    const abort = new AbortController()
    collection<T>(path, abort.signal)
      .then((items) => {
        if (!abort.signal.aborted) setResult({ path, items })
      })
      .catch((error) => {
        if (!abort.signal.aborted) setResult({ path, error })
      })
    return () => abort.abort()
  }, [path, revision])
  return result?.path === path ? result : {}
}
