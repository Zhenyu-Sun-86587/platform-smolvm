// Invoked by console_machine_kvm.py inside its disposable network namespace.
import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import { basename, join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"

const [scenario, origin, machineOrRuntime, network] = process.argv.slice(2)
assert.equal(new URL(origin).hostname, "127.0.0.1")
const { chromium } = await import(process.env.PLAYWRIGHT_CORE || "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM })
const page = await browser.newPage({ viewport: { width: 1117, height: 897 } })
// B owns the platformd candidate. Serve A's built UI at its origin while all API, event
// streams and WebSockets use the running binary, including its normal session/CSRF checks.
const dist = process.env.CONSOLE_DIST
if (dist) {
  await page.route(origin + "/", route => route.fulfill({ path: join(dist, "index.html") }))
  await page.route(origin + "/assets/*", route => {
    const name = basename(new URL(route.request().url()).pathname)
    return /^[\w.-]+$/.test(name) ? route.fulfill({ path: join(dist, "assets", name) }) : route.abort()
  })
}
page.setDefaultTimeout(25000)
const errors = []
page.on("pageerror", e => errors.push(e.message))
const networkStatus = []
const debug = await page.context().newCDPSession(page)
await debug.send("Network.enable")
debug.on("Network.webSocketWillSendHandshakeRequest", ({ request }) => {
  const names = Object.fromEntries(Object.entries(request.headers).map(([k, v]) => [k.toLowerCase(), v]))
  const protocols = (names["sec-websocket-protocol"] || "").split(",").map(value => value.trim())
  networkStatus.push(["handshake", names.origin === origin, protocols[0], protocols.some(p => p.startsWith("ticket."))])
})
debug.on("Network.webSocketHandshakeResponseReceived", ({ response }) => networkStatus.push(["upgrade", response.status]))
page.on("response", r => {
  const url = new URL(r.url())
  if (r.status() >= 400 || url.pathname.includes("tunnel-ticket"))
    networkStatus.push([r.request().method(), url.pathname, r.status()])
})
page.on("websocket", ws => {
  const route = new URL(ws.url()).pathname
  networkStatus.push(["websocket", route])
  ws.on("close", () => networkStatus.push(["closed", route]))
  ws.on("socketerror", message => {
    // The server never places credentials in the socket URL. Remove any subprotocol ticket or query in browser diagnostics.
    const summary = String(message).replace(/ticket\.[^\s,'"]+/g, "[ticket]").replace(/\?[^\s'"]+/g, "").slice(0, 250)
    networkStatus.push(["socketError", route, summary])
  })
})
const path = "/v1/machines/" + encodeURIComponent(machineOrRuntime)
const machineHash = "#/machines/" + encodeURIComponent(machineOrRuntime)
const keyName = "Console verification"

async function screenshot(name) {
  if (process.env.CONSOLE_EVIDENCE)
    await page.screenshot({ path: join(process.env.CONSOLE_EVIDENCE, name + ".png"), animations: "disabled" })
}

async function visitSection(name) {
  await page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name, exact: true }).click()
}

async function get(pathname) {
  const response = await page.request.get(origin + pathname)
  assert.ok(response.ok(), "GET " + pathname + " returned " + response.status())
  return response.json()
}

async function createMachine() {
  await page.getByRole("button", { name: /new machine/i }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByPlaceholder("my-machine").fill(keyName)
  await dialog.getByRole("combobox", { name: /^Template\b/ }).selectOption("t_console_desktop")
  await dialog.getByRole("combobox", { name: /^Runtime\b/ }).selectOption(machineOrRuntime)
  await dialog.getByRole("button", { name: "Custom size" }).click()
  await dialog.getByLabel("vCPUs", { exact: true }).fill("1")
  await dialog.getByLabel("Memory (MiB)", { exact: true }).fill("512")
  await dialog.getByText("Advanced options", { exact: true }).click()
  await dialog.getByRole("combobox", { name: /^Private network\b/ }).selectOption(network)
  await dialog.getByRole("combobox", { name: /^Network mode\b/ }).selectOption("native")
  const accepted = page.waitForResponse(r => r.request().method() === "POST" && new URL(r.url()).pathname === "/v1/machines")
  await dialog.getByRole("button", { name: "Create", exact: true }).click()
  const response = await accepted
  assert.equal(response.status(), 202)
  const receipt = await response.json()
  await dialog.getByRole("link", { name: /^(open|view) machine$/i }).waitFor({ timeout: 160000 })
  const op = await get("/v1/operations/" + receipt.operation_id)
  assert.equal(op.state, "succeeded")
  const vm = await get("/v1/machines/" + op.machine_id)
  assert.equal(vm.name, keyName)
  assert.equal(vm.power_state, "running")
  return { machine_id: vm.machine_id, operation_id: op.operation_id, createStatus: response.status(), operation: op.state }
}

async function terminal() {
  const shell = page.getByRole("dialog").last()
  await shell.getByRole("button", { name: "Open " + keyName, exact: true }).click()
  let output = ""
  let resize = false
  let count = 0
  page.on("websocket", socket => {
    if (!socket.url().includes("/attach?")) return
    count++
    socket.on("framereceived", ({ payload }) => { if (Buffer.isBuffer(payload)) output += payload.toString() })
    socket.on("framesent", ({ payload }) => {
      if (typeof payload === "string" && JSON.parse(payload).type === "resize") resize = true
    })
  })
  const started = page.waitForResponse(r => r.request().method() === "POST" && new URL(r.url()).pathname === path + "/execs")
  await page.getByRole("button", { name: /Web terminal/i }).click()
  const response = await started
  assert.ok(response.ok(), "Start PTY returned " + response.status())
  const { exec_id } = await response.json()
  await page.getByText("Connected", { exact: true }).first().waitFor()
  assert.ok(new URL(page.url()).hash.endsWith("/" + exec_id))
  await page.setViewportSize({ width: 860, height: 700 })
  const input = page.locator(".xterm-helper-textarea")
  await input.focus()
  await page.keyboard.type("printf 'console-%s\\n' verified")
  await page.keyboard.press("Enter")
  for (let i = 0; i < 60 && !output.includes("console-verified"); i++) await delay(150)
  assert.ok(output.includes("console-verified"), "Missing shell output")
  assert.ok(resize, "Missing PTY resize")
  await screenshot("live-terminal")
  await page.reload()
  await page.getByText("Connected", { exact: true }).first().waitFor()
  assert.ok(new URL(page.url()).hash.endsWith("/" + exec_id))
  assert.equal(count, 2, "Refresh must attach to the same exec")
  await page.locator(".xterm-helper-textarea").focus()
  await page.keyboard.type("exit 7")
  await page.keyboard.press("Enter")
  await page.getByText(/Session exited \(7\)/).waitFor()
  assert.equal((await get(path + "/execs")).items.filter(item => item.exec_id === exec_id).length, 1)
  return { exec_id, attachCount: count, resize, shellOutput: true, exit: 7 }
}

async function files() {
  await page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name: "Files" }).click()
  const section = page.getByRole("region", { name: "Guest files" })
  await section.getByLabel("Guest directory").fill("/tmp")
  await section.getByRole("button", { name: "Go", exact: true }).click()
  await section.locator("footer code").filter({ hasText: /^\/tmp$/ }).waitFor()
  const name = "console-reference.dat"
  const contents = Buffer.from([0, 255, 13, 10, 128, 65, 1])
  const textFile = "console-unicode.txt"
  for (const [file, bytes] of [[name, contents], [textFile, Buffer.from("smolvm → control\n")]]) {
    await section.getByRole("button", { name: "Upload", exact: true }).click()
    const form = page.getByRole("dialog", { name: "Upload file", exact: true })
    await form.getByLabel("Local file").setInputFiles({ name: file, mimeType: "application/octet-stream", buffer: bytes })
    const response = page.waitForResponse(r => new URL(r.url()).pathname === path + "/files" && r.request().method() !== "GET")
    await form.getByRole("button", { name: "Upload", exact: true }).click()
    const uploaded = await response
    assert.ok(uploaded.ok(), "Upload returned " + uploaded.status())
    const receipt = await uploaded.json()
    assert.equal(receipt.sha256, createHash("sha256").update(bytes).digest("hex"))
    await section.getByRole("button", { name: file, exact: true }).waitFor()
  }
  await section.getByRole("button", { name: name, exact: true }).click()
  const downloading = page.waitForEvent("download")
  await section.getByRole("button", { name: "Download" }).click()
  const download = await downloading
  assert.equal(download.suggestedFilename(), name)
  assert.deepEqual(readFileSync(await download.path()), contents)
  await section.getByRole("button", { name: textFile, exact: true }).click()
  await section.getByRole("button", { name: "Text", exact: true }).click()
  const editor = page.getByRole("dialog", { name: "Edit text file" })
  assert.equal(await editor.getByLabel("File contents").inputValue(), "smolvm → control\n")
  const edited = "校验通过\n"
  await editor.getByLabel("File contents").fill(edited)
  await editor.getByRole("button", { name: "Save", exact: true }).click()
  await editor.waitFor({ state: "hidden" })
  // A separate read confirms the saved bytes rather than the editor's local state.
  const read = await page.request.get(origin + path + "/files?path=" + encodeURIComponent("/tmp/" + textFile))
  assert.equal(read.status(), 200)
  assert.equal(await read.text(), edited)
  await screenshot("live-files")
  return { binaryBytes: contents.length, binarySha256: createHash("sha256").update(contents).digest("hex"), utf8Edit: true }
}

async function desktop() {
  await visitSection("Networking")
  const service = (await get(path + "/services")).items.find(item => item.protocol === "vnc")
  assert.ok(service && !service.public_port)
  await page.getByText(service.name, { exact: true }).first().waitFor()
  await screenshot("live-networking")
  await visitSection("Overview")
  await page.getByRole("dialog").last().getByRole("button", { name: "Open " + keyName, exact: true }).click()
  await page.getByRole("link", { name: /Desktop.*Guest display/i }).click()
  const opened = page.waitForResponse(r => r.request().method() === "POST" && new URL(r.url()).pathname === path + "/desktop-sessions")
  await page.getByRole("button", { name: "Connect", exact: true }).click()
  const response = await opened
  assert.ok(response.ok(), "Open desktop session returned " + response.status())
  const session = await response.json()
  await page.getByText("Connected", { exact: true }).waitFor()
  const surface = page.locator('[aria-label="Remote desktop"] canvas')
  await surface.waitFor()
  await page.waitForFunction(() => {
    const canvas = document.querySelector('[aria-label="Remote desktop"] canvas')
    return canvas?.width === 800 && canvas.height === 480
  })
  const vm = await get(path)
  await screenshot("live-desktop")
  assert.equal(session.execution_epoch, vm.binding.execution_epoch)
  assert.equal(session.binding_id, vm.binding.binding_id)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.reload()
  await page.getByRole("button", { name: "Reconnect", exact: true }).click()
  await page.getByText("Connected", { exact: true }).waitFor()
  const sessions = (await get(path + "/desktop-sessions")).items
  assert.deepEqual(sessions.filter(item => item.state === "active").map(item => item.desktop_session_id), [session.desktop_session_id])
  await page.getByRole("button", { name: "End session" }).click()
  await page.getByText("Session closed", { exact: true }).waitFor()
  assert.ok(!(await get(path + "/desktop-sessions")).items.some(item => item.state === "active"))
  return { width: 800, height: 480, reusedSession: true, closed: true, execution_epoch: session.execution_epoch }
}

async function observations() {
  // Re-enter Overview so this assertion compares the rendered sample to that exact response.
  await visitSection("Networking")
  const samples = new Map()
  page.on("response", async response => {
    if (new URL(response.url()).pathname !== path + "/metrics" || !response.ok()) return
    const data = await response.json().catch(() => null)
    if (data?.observed_at) samples.set(data.observed_at, data)
  })
  await visitSection("Overview")
  const metrics = page.getByRole("region", { name: /Resource usage/ })
  // The retained list can also sample this machine. Match the displayed observation time.
  await metrics.locator("time[datetime]").first().waitFor()
  let rendered, sample
  for (let index = 0; index < 100; index++) {
    rendered = await metrics.evaluate(el => ({
      at: el.querySelector("time")?.getAttribute("datetime"),
      values: [...el.querySelectorAll("h3")].map(heading => heading.parentElement.querySelector("strong")?.textContent),
    }))
    sample = samples.get(rendered.at)
    if (sample) break
    await delay(50)
  }
  assert.ok(sample, "No response matched the visible metrics sample " + JSON.stringify(rendered))
  assert.ok(["available", "partial"].includes(sample.availability))
  const vm = await get(path)
  assert.equal(sample.execution.binding_id, vm.binding.binding_id)
  const values = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 })
  const expected = [["cpu_millis", "ms"], ["memory_mb", "MB"], ["disk_used_mb", "MB"]].map(([field, unit]) =>
    sample[field] == null || sample.issues?.[field] ? "Unavailable" : values.format(sample[field]) + " " + unit)
  assert.deepEqual(rendered.values, expected, "Metric tiles disagree with the displayed observation")
  await screenshot("live-metrics")
  await visitSection("Checkpoints")
  await page.getByRole("button", { name: "Checkpoint", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Capture VM checkpoint" })
  const label = "Console capture"
  await dialog.getByLabel("Label", { exact: true }).fill(label)
  const receipt = page.waitForResponse(r => new URL(r.url()).pathname === path + "/snapshots" && r.request().method() === "POST")
  await dialog.getByRole("button", { name: "Capture", exact: true }).click()
  const response = await receipt
  assert.equal(response.status(), 202)
  const operation = await response.json()
  await page.getByText(label, { exact: true }).first().waitFor({ timeout: 120000 })
  const { items } = await get(path + "/snapshots")
  const snapshot = items.find(item => item.label === label)
  assert.equal(snapshot?.capture_summary?.configuration?.status, "verified")
  assert.equal((await get("/v1/operations/" + operation.operation_id)).state, "succeeded")
  const captured = snapshot.capture_summary.configuration.spec.resources
  await page.getByText("Captured configuration", { exact: true }).click()
  const detail = page.locator("details[open]").filter({ hasText: "Captured configuration" })
  for (const [title, value] of [["CPU", captured.cpu_count], ["Memory MiB", captured.memory_mib]]) {
    const row = detail.getByText(title, { exact: true }).locator("..")
    assert.equal(await row.locator("dd").textContent(), String(value))
  }
  await screenshot("live-checkpoint")
  return { availability: sample.availability, scope: sample.scope, source: sample.source, execution_epoch: sample.execution.execution_epoch,
    snapshot: snapshot.snapshot_id, cpu_count: captured.cpu_count, memory_mib: captured.memory_mib, capture: "verified" }
}

try {
  await page.goto(origin + "/" + (scenario === "create" ? "#/machines" : machineHash))
  await page.getByLabel("Username").fill("owner")
  await page.getByLabel("Password").fill(readFileSync(0, "utf8"))
  await page.getByRole("button", { name: "Sign in" }).click()
  assert.ok(["create", "guest", "desktop", "observe"].includes(scenario))
  let result
  if (scenario === "create") result = await createMachine()
  else if (scenario === "desktop") result = await desktop()
  else if (scenario === "observe") result = await observations()
  else {
    const fileResult = await files()
    await page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name: "Overview" }).click()
    result = { files: fileResult, terminal: await terminal() }
  }
  assert.deepEqual(errors, [])
  console.log(JSON.stringify(result))
} catch (error) {
  if (scenario === "create") {
    console.error("Create form at failure:", (await page.getByRole("dialog").first().innerText({ timeout: 1000 }).catch(() => "")).slice(0, 2000))
    console.error("Visible alerts:", (await page.getByRole("alert").allTextContents()).slice(0, 3))
  }
  if (scenario !== "create") {
    console.error("Route:", new URL(page.url()).hash, "Status:", await page.getByRole("status").allTextContents(),
      "Alerts:", await page.getByRole("alert").allTextContents(), "Errors:", errors, "Network:", networkStatus.slice(-15))
    if (scenario === "desktop") {
      const vm = await get(path)
      const { items } = await get(path + "/desktop-sessions")
      console.error("Desktop bindings:", items.map(s => [s.state, s.binding_id, s.binding_generation, s.execution_epoch]),
        "Machine binding:", [vm.binding?.binding_id, vm.binding_generation, vm.binding?.execution_epoch])
    }
    if (scenario === "observe") console.error("Metric region:", await page.getByRole("region", { name: /Resource usage/ }).allTextContents())
  }
  throw error
} finally {
  await browser.close()
}
