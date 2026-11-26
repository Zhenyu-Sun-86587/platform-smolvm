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
assert.ok(dist)
await page.route(origin + "/**", route => {
  const url = new URL(route.request().url())
  if (url.pathname === "/") return route.fulfill({ path: join(dist, "index.html") })
  if (/^\/assets\/[\w.-]+$/.test(url.pathname))
    return route.fulfill({ path: join(dist, "assets", basename(url.pathname)) })
  return route.continue()
})
page.setDefaultTimeout(25000)
const errors = []
page.on("pageerror", e => errors.push(e.message))
const path = "/v1/machines/" + encodeURIComponent(machineOrRuntime)
const machineHash = "#/machines/" + encodeURIComponent(machineOrRuntime)
const keyName = "Console verification"

async function get(pathname) {
  const response = await page.request.get(origin + pathname)
  assert.ok(response.ok(), "GET " + pathname + " returned " + response.status())
  return response.json()
}

async function createMachine() {
  await page.getByRole("button", { name: /new machine/i }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByPlaceholder("my-machine").fill(keyName)
  await dialog.getByLabel("Template", { exact: true }).selectOption("t_console_desktop")
  await dialog.getByLabel("Runtime", { exact: true }).selectOption(machineOrRuntime)
  await dialog.getByRole("button", { name: "Custom size" }).click()
  await dialog.getByLabel("vCPUs", { exact: true }).fill("1")
  await dialog.getByLabel("Memory (MiB)", { exact: true }).fill("512")
  await dialog.getByText("Advanced options", { exact: true }).click()
  await dialog.getByLabel("Private network", { exact: true }).selectOption(network)
  await dialog.getByLabel("Network mode", { exact: true }).selectOption("native")
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
  return { binaryBytes: contents.length, binarySha256: createHash("sha256").update(contents).digest("hex"), utf8Edit: true }
}

async function desktop() {
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

try {
  await page.goto(origin + "/" + (scenario === "create" ? "#/machines" : machineHash))
  await page.getByLabel("Username").fill("owner")
  await page.getByLabel("Password").fill(readFileSync(0, "utf8"))
  await page.getByRole("button", { name: "Sign in" }).click()
  assert.ok(["create", "guest", "desktop"].includes(scenario))
  let result
  if (scenario === "create") result = await createMachine()
  else if (scenario === "desktop") result = await desktop()
  else {
    const fileResult = await files()
    await page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name: "Overview" }).click()
    result = { files: fileResult, terminal: await terminal() }
  }
  assert.deepEqual(errors, [])
  console.log(JSON.stringify(result))
} finally {
  await browser.close()
}
