import { useState } from "react"
import {
  Bot, Braces, Building2, Cloud, Code2, Copy, Database, GitBranch, Globe2, Link2, LockKeyhole, Plus, Search,
} from "lucide-react"
import { fixtureScene } from "../../api"
import { Button, EmptyState, Modal, PageHeader } from "../../ui"
import { CommandPreview } from "../catalog/CommandPreview"
import { DocumentationLink } from "../catalog/DocumentationLink"
import catalog from "../catalog/catalog.module.css"
import styles from "./settings.module.css"

type PreviewPage = "agents" | "integrations" | "domains" | "environment"
const pages = {
  agents: { title: "Agents", Icon: Bot, description: "How your coding agents’ turns went, across your machines, for the last 30 days. Reading them never wakes a machine." },
  integrations: { title: "Integrations", Icon: Link2, description: "Connect an account once and it’s available inside every machine. Agents and scripts can use these connections." },
  domains: { title: "Domains", Icon: Globe2, description: "Bring your own domain, point it at any of your machines." },
  environment: { title: "Environment", Icon: LockKeyhole, description: "Variables and secrets injected into your VMs’ commands." },
}
const integrations = [
  { name: "Claude Code", category: "Coding", Icon: Code2, description: "Coding agent for terminal workspaces." },
  { name: "Codex", category: "Coding", Icon: Bot, description: "Coding agent for your workspace." },
  { name: "Antigravity", category: "Coding", Icon: Cloud, description: "Developer tools for agent-led work." },
  { name: "Git", category: "Source control", Icon: GitBranch, description: "Distributed version control for source code." },
  { name: "GitHub", category: "Source control", Icon: GitBranch, description: "Repository access for your account." },
  { name: "GitHub App", category: "Source control", Icon: GitBranch, description: "Repository access for an organization." },
  { name: "Figma", category: "Design", Icon: Braces, description: "Design files and collaboration." },
  { name: "Linear", category: "Planning", Icon: Code2, description: "Project and issue tracking." },
  { name: "Neon", category: "Databases", Icon: Database, description: "Hosted PostgreSQL projects." },
]

export function PreviewPage({ page }: { page: PreviewPage }) {
  const [open, setOpen] = useState(false)
  const [region, setRegion] = useState("fra")
  const [agent, setAgent] = useState("claude-code")
  const { title, Icon, description } = pages[page]
  const addLabel = page === "domains" ? "Add domain" : "Add variable"
  if (!fixtureScene) return <>
    <PageHeader title={title} icon={<Icon />} />
    <EmptyState icon={<Icon />} title="Unavailable on this platform" />
    {page === "domains" && <p>Manage published hostnames in <a href="#/networking">Services</a>.</p>}
  </>
  return <>
    <PageHeader title={title} icon={<Icon />} description={description} className={page === "integrations" ? catalog.stackedHeading : undefined}
      actions={page !== "agents" && <><DocumentationLink />
        {(page === "domains" || page === "environment") && <Button variant="primary" onClick={() => setOpen(true)}><Plus size={15} /> {addLabel}</Button>}</>} />
    {page === "integrations" ? <IntegrationGrid /> : page === "agents" ? <>
      <div className={catalog.toolbar}><select className={catalog.scope} aria-label="Machine scope"><option>All machines</option><option>atlas-dev</option></select></div>
      <section className={catalog.featurePanel} data-split="true">
        <div><h2>No agent runs yet</h2><p>Finished coding agent turns appear here when agent sessions are available.</p></div>
        <div><span className={styles.eyebrow}>Agent sessions</span><pre>{`Start a session\n\nMachine    atlas-dev\nAgent      Coding assistant\nStatus     Idle`}</pre></div>
      </section>
    </> : page === "domains" ? <>
      <p className={styles.wildcard}>An organization-wide wildcard domain can cover all machines in a region. <a href="#/settings">View settings</a></p>
      <div className={catalog.toolbar}><div className={catalog.actions}><select className={catalog.regionSelect} aria-label="Region" value={region} onChange={event => setRegion(event.target.value)}>
        <option value="fra">Frankfurt</option><option value="nyc">New York</option>
      </select></div></div>
      <p className={catalog.empty}>No domains in this region.</p>
    </> : <section className={catalog.featurePanel} data-split="true">
      <div><h2>Add a variable or secret</h2><p>Workspace variables and secrets appear here when this capability is available.</p>
        <Button variant="primary" onClick={() => setOpen(true)}>Add variable</Button></div>
      <div><span className={styles.eyebrow}>Example</span><pre>{`DATABASE_URL=postgres://…\nAPI_TOKEN=••••••••`}</pre></div>
    </section>}
    {open && <Modal open onOpenChange={value => { if (!value) setOpen(false) }} title={addLabel} className={catalog.smallModal}
      description={page === "domains" ? "Custom domain setup is available as a preview." : "Workspace environment settings are available as a preview."}
      footer={<Button onClick={() => setOpen(false)}>Close</Button>}>
      <div className={catalog.form}>{page === "domains" ? <label>Hostname<input placeholder="app.example.test" /></label> : <>
        <label>Name<input placeholder="APP_REGION" /></label><label>Value<input placeholder="local" /></label></>}</div>
    </Modal>}
  </>
}

function IntegrationGrid() {
  const [query, setQuery] = useState("")
  const [category, setCategory] = useState("")
  const [sort, setSort] = useState("recommended")
  const [detail, setDetail] = useState<typeof integrations[number]>()
  const shown = integrations.filter(item => (!category || item.category === category) && item.name.toLowerCase().includes(query.trim().toLowerCase()))
  if (sort === "name") shown.sort((first, second) => first.name.localeCompare(second.name))
  return <>
    <div className={styles.integrationToolbar}>
      <strong>Available <span>{shown.length}</span></strong>
      <label><Search size={14} /><input aria-label="Search integrations" placeholder="Search integrations…" value={query} onChange={event => setQuery(event.target.value)} /></label>
      <select aria-label="Category" value={category} onChange={event => setCategory(event.target.value)}>
        <option value="">All categories</option>{Array.from(new Set(integrations.map(item => item.category))).map(item => <option key={item}>{item}</option>)}
      </select>
      <select aria-label="Sort integrations" value={sort} onChange={event => setSort(event.target.value)}><option value="recommended">Recommended</option><option value="name">Name</option></select>
    </div>
    <div className={styles.integrationGrid}>
      {shown.map(({ name, category, Icon, description }) => <button type="button" key={name} onClick={() => setDetail({ name, category, Icon, description })}>
        <Icon size={36} /><small>Recommended</small><strong>{name}</strong><p>{description}</p>
      </button>)}
    </div>
    {!shown.length && <p className={catalog.empty}>No matching integrations.</p>}
    {detail && <Modal open onOpenChange={value => { if (!value) setDetail(undefined) }} title={detail.name}
      description={detail.description} footer={<Button onClick={() => setDetail(undefined)}>Close</Button>}>
      <p>Connections are unavailable in preview.</p>
    </Modal>}
  </>
}

const sections = [["", "General"], ["members", "Members"], ["teams", "Teams"], ["billing", "Billing"], ["usage", "Usage"]]
export function OrganizationPreview({ tab }: { tab?: string }) {
  const [create, setCreate] = useState(false)
  const [edit, setEdit] = useState(false)
  const [copied, setCopied] = useState("")
  const orgID = "org_example-workspace"
  async function copyID() {
    try {
      await navigator.clipboard.writeText(orgID)
      setCopied("Copied")
    } catch {
      setCopied("Copy unavailable")
    }
  }
  return <>
    <PageHeader title="Organization settings" icon={<Building2 />} description="Identity, members, billing, and settings for your organization." actions={<DocumentationLink />} />
    <nav className={styles.tabs} aria-label="Settings sections">{sections.map(([id, title]) => <a key={title} href={"#/settings" + (id ? "/" + id : "")}
      aria-current={(tab ?? "") === id ? "page" : undefined}>{title}</a>)}</nav>
    {tab === "teams" ? <>
      <div className={styles.sectionToolbar}><span className={styles.eyebrow}>0 teams</span><Button className={catalog.neutralPrimary} variant="primary" onClick={() => setCreate(true)}>Create team</Button></div>
      <section className={catalog.featurePanel}><div><h2>No teams yet</h2><p>Create a team to group organization members.</p></div></section>
    </> : !tab ? <>
      <section className={styles.previewSection}>
        <h2>Identity</h2><p>Find the organization name and ID for API and CLI calls.</p>
        <div className={styles.identityCard}>
          <button className={styles.editName} type="button" onClick={() => setEdit(true)}>Edit</button>
          <span className={styles.eyebrow}>Organization name</span><strong>Example Workspace</strong>
          <span className={styles.eyebrow}>Organization ID</span>
          <div className={styles.identityId}><code>{orgID}</code><button type="button" aria-label="Copy organization ID" onClick={copyID}><Copy size={13} /></button>
            {copied && <span role="status">{copied}</span>}</div>
        </div>
      </section>
      <section className={styles.previewSection}><h2>Wildcard domain</h2><p>A wildcard domain covers the machines in a region. Each region can have its own, such as preview.example.test for Frankfurt.</p>
        <div className={styles.wildcardRows}><span><small>FRA</small> No wildcard domain</span><span><small>NYC</small> No wildcard domain</span></div></section>
    </> : <EmptyState title={`${sections.find(([id]) => id === tab)?.[1] ?? "Settings"} preview`} />}
    {edit && <Modal open onOpenChange={value => { if (!value) setEdit(false) }} title="Edit organization name"
      description="Organization identity is available as a preview." footer={<Button onClick={() => setEdit(false)}>Close</Button>}>
      <div className={catalog.form}><label>Organization name<input defaultValue="Example Workspace" /></label></div>
    </Modal>}
    {create && <Modal open onOpenChange={value => { if (!value) setCreate(false) }} title="Create team"
      description="Team management is available as a preview." footer={<Button onClick={() => setCreate(false)}>Close</Button>}>
      <div className={catalog.form}><label>Team name<input placeholder="Engineering" /></label></div>
    </Modal>}
  </>
}
