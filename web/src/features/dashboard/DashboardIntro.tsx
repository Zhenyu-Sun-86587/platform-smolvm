import { useState } from "react"
import { ChevronLeft, ChevronRight, X } from "lucide-react"
import { Button, Modal } from "../../ui"
import { PreviewBrandLogo } from "../catalog/PreviewBrandLogo"
import styles from "./dashboard.module.css"

const agents = [
  { name: "Claude Code", logo: "claude" },
  { name: "Codex", logo: "codex" },
  { name: "Antigravity", logo: "antigravity" },
] as const
const tips = [
  { category: "Coding agents", title: "Connect your coding agent",
    description: "Sign in once and Claude Code, Codex or Antigravity is signed in on all your private machines." },
  { category: "Integrations", title: "Connect your accounts",
    description: "Browse connections that make your tools available to machines and agents.", href: "#/integrations" },
  { category: "Command line", title: "Manage machines from the CLI",
    description: "Use the CLI to create machines, list running guests, and inspect operations.", command: "sandboxctl machines list" },
  { category: "Connect", title: "SSH into a machine",
    description: "If you have manage access, you can use this command on machines that support managed SSH. Open the machine’s connection options to check.", command: "sandboxctl ssh MACHINE" },
  { category: "Reusable images", title: "Create a reusable snapshot",
    description: "Capture a machine as a snapshot to preserve a restore point.", href: "#/snapshots" },
]

export function DashboardIntro() {
  const [visible, setVisible] = useState(true)
  const [position, setPosition] = useState(0)
  const [agent, setAgent] = useState("")
  if (!visible) return null
  const tip = tips[position]
  function step(offset: number) {
    setPosition(value => (value + tips.length + offset) % tips.length)
  }
  return <section className={styles.intro} aria-label="Workspace tips">
    <button type="button" className={styles.dismiss} aria-label="Dismiss" onClick={() => setVisible(false)}><X size={14} /></button>
    <div>
      <span className={styles.tipCategory}>{tip.category}</span>
      <h2>{tip.title}</h2>
      <p>{tip.description}</p>
      {position === 0 && <div className={styles.agentCards}>{agents.map(({ name, logo }) =>
        <div className={styles.example} key={name}>
          <PreviewBrandLogo logo={logo} size={18} /><strong>{name}</strong>
          <Button size="sm" onClick={() => setAgent(name)} aria-label={`Connect ${name}`}>Connect</Button>
        </div>)}</div>}
      {tip.href && <a href={tip.href}>View {tip.category.toLowerCase()} <span aria-hidden="true">→</span></a>}
      {tip.command && <pre>{tip.command}</pre>}
    </div>
    <div className={styles.tipNavigation}>
      <button type="button" aria-label="Previous tip" onClick={() => step(-1)}><ChevronLeft size={14} /></button>
      <div>{tips.map(({ title }, index) => <button type="button" key={title} aria-label={title}
        aria-current={position === index ? "true" : undefined} onClick={() => setPosition(index)} />)}</div>
      <button type="button" aria-label="Next tip" onClick={() => step(1)}><ChevronRight size={14} /></button>
    </div>
    {agent && <Modal open onOpenChange={open => { if (!open) setAgent("") }} title={`Connect ${agent}`}
      description="Agent connections are shown in the reference preview."
      footer={<Button onClick={() => setAgent("")}>Close</Button>}><p>Sign-in is unavailable in this deployment.</p></Modal>}
  </section>
}
