// Exercise the embedded production UI against a new platformd registry on loopback.
// PLAYWRIGHT_CORE and CHROMIUM may point at an existing local browser installation.
import assert from "node:assert/strict"
import { spawn } from "node:child_process"
import { randomBytes } from "node:crypto"
import { once } from "node:events"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { createServer } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"

const platformd = process.argv[2]
assert.ok(platformd, "Pass the platformd binary built with the candidate web/dist")
const { chromium } = await import(process.env.PLAYWRIGHT_CORE || "playwright-core")
const root = await mkdtemp(join(tmpdir(), "sandbox-console-test-"))
const reservation = createServer().listen(0, "127.0.0.1")
await once(reservation, "listening")
const { port } = reservation.address()
await new Promise(resolve => reservation.close(resolve))
const origin = "http://127.0.0.1:" + port
const password = randomBytes(24).toString("hex")
const secretFile = join(root, "owner-password")
const cfg = join(root, "config.json")
await writeFile(secretFile, password, { mode: 0o600 })
await writeFile(cfg, JSON.stringify({
  version: 1, state_dir: join(root, "registry"), api: { listen: "127.0.0.1:" + port, public_origin: origin },
  runtime: { socket: join(root, "offline.sock"), status_socket: "", data_dir: join(root, "runtime") },
}))
const server = spawn(platformd, ["--config", cfg], {
  env: { ...process.env, SANDBOX_OWNER_PASSWORD_FILE: secretFile, SANDBOX_OWNER_USER: "owner" },
  stdio: ["ignore", "ignore", "inherit"],
})
const stopped = once(server, "exit")
let browser
try {
  for (let attempt = 0; attempt < 100; attempt++) {
    assert.equal(server.exitCode, null, "platformd exited during startup")
    if (await fetch(origin + "/v1/control").then(r => r.status === 401).catch(() => false)) break
    await delay(100)
  }
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM })
  const page = await browser.newPage()
  page.setDefaultTimeout(7000)
  const errors = []
  page.on("pageerror", e => errors.push(e.message))
  await page.goto(origin + "/?fixture=02#/api-keys")
  // A dev scene query on a production build must still authenticate.
  await page.getByLabel("Username").fill("owner")
  await page.getByLabel("Password").fill(password + "x")
  const [rejected] = await Promise.all([page.waitForResponse(r => r.url().endsWith("/v1/auth/login")),
    page.getByRole("button", { name: "Sign in" }).click()])
  assert.equal(rejected.status(), 401)
  await page.getByLabel("Password").fill(password)
  await page.getByRole("button", { name: "Sign in" }).click()
  const create = page.getByRole("button", { name: /create (api )?(key|token)/i }).first()
  await create.click()
  const dialog = page.getByRole("dialog")
  const label = "console-e2e-read"
  await dialog.getByLabel(/label|name/i).first().fill(label)
  const scope = dialog.getByLabel(/machine id/i)
  await scope.fill("m_missing")
  const invalidScope = page.waitForResponse(r => r.request().method() === "POST" && r.url().endsWith("/v1/tokens"))
  await dialog.locator('button[type="submit"]').click()
  assert.equal((await invalidScope).status(), 404)
  await dialog.getByRole("alert").waitFor()
  assert.equal(await dialog.getByLabel("Label", { exact: true }).inputValue(), label)
  await scope.fill("")
  const issuedResponse = page.waitForResponse(r => new URL(r.url()).pathname === "/v1/tokens" && r.request().method() === "POST")
  await dialog.locator('button[type="submit"]').click()
  const issued = await issuedResponse
  assert.ok(issued.ok(), "Issue token returned " + issued.status())
  const receipt = await issued.json()
  assert.ok(receipt.token && receipt.token_id && receipt.revision, "Incomplete issuance receipt")
  await page.waitForFunction(secret => document.body.innerText.includes(secret) ||
    Array.from(document.querySelectorAll("input,textarea")).some(el => el.value === secret), receipt.token)
  await page.reload()
  await page.getByText(label, { exact: true }).first().waitFor()
  const listed = await page.request.get(origin + "/v1/tokens")
  const listing = await listed.json()
  assert.ok(listing.items.some(item => item.token_id === receipt.token_id))
  assert.ok(!JSON.stringify(listing).includes(receipt.token))
  assert.ok(!(await page.locator("body").innerText()).includes(receipt.token))
  const revoke = page.getByRole("button", { name: /revoke/i }).first()
  await revoke.click()
  await dialog.getByRole("button", { name: /cancel/i }).click()
  assert.ok((await (await page.request.get(origin + "/v1/tokens")).json()).items.some(t => t.token_id === receipt.token_id && !t.revoked_at))
  await revoke.click()
  const revokedResponse = page.waitForResponse(r => r.url().includes("/v1/tokens/") && r.request().method() === "DELETE")
  await dialog.locator('button[type="submit"]').click()
  const revoked = await revokedResponse
  assert.ok(revoked.ok(), "Revoke returned " + revoked.status())
  const remaining = (await (await page.request.get(origin + "/v1/tokens")).json()).items
  assert.ok(!remaining.some(t => t.token_id === receipt.token_id && !t.revoked_at))
  assert.deepEqual(errors, [])
  console.log(JSON.stringify({ browser: browser.version(), productionFixtureRequiresLogin: true,
    rejectedLogin: rejected.status(), missingMachineScope: 404, issue: issued.status(), oneTimeSecret: true, cancelPreservedToken: true,
    revoke: revoked.status(), consoleErrors: errors }))
} finally {
  await browser?.close()
  if (server.exitCode === null) server.kill("SIGTERM")
  await stopped
  await rm(root, { recursive: true, force: true })
}
