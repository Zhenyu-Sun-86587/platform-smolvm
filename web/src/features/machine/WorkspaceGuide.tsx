import { useState } from "react"
import type { ReactNode } from "react"
import { Check, Copy } from "lucide-react"
import { Callout } from "../../common"
import styles from "./detail.module.css"

export function WorkspaceGuide({ label, title, description, actions, commands }: {
  label: string; title: string; description: string; actions?: ReactNode; commands?: readonly string[]
}) {
  return <section className={styles.guide} data-split={Boolean(commands?.length)}>
    <div className={styles.guideBody}>
      <span className={styles.guideLabel}>{label}</span>
      <h2>{title}</h2>
      <p>{description}</p>
      {actions && <div className={styles.guideActions}>{actions}</div>}
    </div>
    {commands?.length ? <CommandPanel commands={commands} /> : null}
  </section>
}

export function CommandPanel({ commands }: { commands: readonly string[] }) {
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState<unknown>()
  return <div className={styles.commandPanel}>
    <header>
      <span>CLI</span>
      <button type="button" aria-label="Copy commands" onClick={() => {
        navigator.clipboard.writeText(commands.filter(line => line && !line.startsWith("#")).join("\n"))
          .then(() => { setCopied(true); setError(undefined) }).catch(setError)
      }}>{copied ? <Check size={12} /> : <Copy size={12} />}{copied ? "Copied" : "Copy"}</button>
    </header>
    <Callout error={error} />
    <pre>{commands.map((line, index) => <span key={index} data-comment={line.startsWith("#")}>{line || " "}</span>)}</pre>
  </div>
}
