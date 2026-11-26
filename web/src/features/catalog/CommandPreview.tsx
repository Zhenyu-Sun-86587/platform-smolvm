import { useState } from "react"
import { Copy } from "lucide-react"
import styles from "./catalog.module.css"

export function CommandPreview({ command, description }: { command: string; description?: string }) {
  const [result, setResult] = useState("")
  async function copy() {
    try {
      await navigator.clipboard.writeText(command)
      setResult("Copied")
    } catch {
      setResult("Copy unavailable")
    }
  }
  return <div className={styles.commandPreview}>
    <header><span>{description ?? "CLI"}</span><button type="button" onClick={copy}><Copy size={13} /> Copy</button></header>
    <pre>{command.split("\n").map(line => line ? "$ " + line : "").join("\n")}</pre>
    {result && <span role="status">{result}</span>}
  </div>
}
