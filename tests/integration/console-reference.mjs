// Capture the local Vite reference scenes with an installed playwright-core and Chromium.
// node console-reference.mjs <vite-origin> <boxd-reference-dir> <output-dir> [scene numbers...]
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import { readdir, mkdir, writeFile } from "node:fs/promises"
import path from "node:path"
import { parseArgs } from "node:util"

const { positionals: [origin, references, output, ...selected] } = parseArgs({ allowPositionals: true })
assert.ok(origin && references && output, "Pass Vite origin, reference directory and output directory")
assert.ok(["127.0.0.1", "localhost", "[::1]"].includes(new URL(origin).hostname), "Use an isolated local Vite server")
const { chromium } = await import(process.env.PLAYWRIGHT_CORE || "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM })
const files = (await readdir(references)).filter(name => /^\d\d-.*\.jpg$/.test(name)).sort()
assert.equal(files.length, 46)
await mkdir(output, { recursive: true })
const results = []

async function checkPalette(page) {
  const trigger = page.getByRole("button", { name: "Open command palette" }).first()
  await trigger.focus()
  await page.keyboard.press("Control+b")
  const field = page.getByRole("combobox", { name: "Search workspace" })
  await field.waitFor()
  assert.ok(await field.evaluate(el => el === document.activeElement), "Palette did not focus its input")
  const options = page.getByRole("option")
  const first = await options.first().getAttribute("id")
  const last = await options.last().getAttribute("id")
  for (const [key, expected] of [["End", last], ["ArrowDown", first], ["ArrowUp", last], ["Home", first]]) {
    await field.press(key)
    assert.equal(await field.getAttribute("aria-activedescendant"), expected, key)
  }
  await field.fill("zero-results-qzx818")
  assert.equal(await options.count(), 0)
  assert.equal(await field.getAttribute("aria-activedescendant"), null)
  await field.press("Enter")
  assert.ok(await field.isVisible(), "No-results Enter executed an old command")
  await field.press("Escape")
  await trigger.waitFor()
  await page.waitForFunction(() => document.activeElement?.getAttribute("aria-label") === "Open command palette")
  await trigger.click()
  await field.fill("Snapshots")
  assert.ok(await options.count() > 0)
  await field.press("Enter")
  await page.waitForURL(/#\/snapshots$/)
}

async function checkDrawer(page) {
  await page.getByRole("button", { name: "Search machines", exact: true }).click()
  const search = page.getByRole("textbox", { name: "Search by name, ID, or template" })
  await search.fill("atlas-dev")
  const link = page.getByRole("link", { name: /atlas-dev.*vCPU/i }).first()
  await link.click()
  const drawer = page.getByRole("dialog").first()
  await drawer.waitFor()
  const view = page.viewportSize()
  assert.ok(Math.abs((await drawer.boundingBox()).width - view.width * .75) < 2, "Desktop detail drawer width")
  await drawer.getByRole("button", { name: /^Open atlas-dev$/ }).click()
  await page.getByRole("dialog", { name: "Open atlas-dev", exact: true }).waitFor()
  await page.keyboard.press("Escape")
  await page.getByRole("dialog", { name: "Open atlas-dev", exact: true }).waitFor({ state: "hidden" })
  await drawer.waitFor({ state: "visible" })
  assert.match(new URL(page.url()).hash, /^#\/machines\/.+/, "Esc dismissed the underlying machine")
  await page.keyboard.press("Escape")
  await drawer.waitFor({ state: "hidden" })
  assert.equal(await search.inputValue(), "atlas-dev")
  await link.click()
  await page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name: "Networking" }).click()
  await page.reload()
  const tab = page.getByRole("navigation", { name: "Machine sections" }).getByRole("link", { name: "Networking" })
  await tab.waitFor()
  assert.equal(await tab.getAttribute("aria-current"), "page")
}

async function capture(file, forceTouch = false) {
  const id = file.slice(0, 2)
  const info = execFileSync("file", ["-b", path.join(references, file)], { encoding: "utf8" })
  const [, w, h] = info.match(/precision 8, (\d+)x(\d+)/)
  const viewport = { width: Number(w), height: Number(h) }
  const touch = forceTouch || viewport.width < 700
  const context = await browser.newContext({
    viewport, deviceScaleFactor: 1, hasTouch: touch, isMobile: touch,
    reducedMotion: "reduce", colorScheme: "dark", locale: "en-US", timezoneId: "UTC",
  })
  const errors = []
  await context.route("**/*", route => {
    const url = new URL(route.request().url())
    if (url.origin !== new URL(origin).origin || url.pathname.startsWith("/v1/")) {
      errors.push("Unexpected request in fixture mode: " + url.origin + url.pathname)
      return route.abort()
    }
    return route.continue()
  })
  const page = await context.newPage()
  page.setDefaultTimeout(7000)
  page.on("pageerror", error => errors.push(error.message))
  const key = id + (forceTouch ? "-coarse" : "")
  const record = { scene: key, reference: file, viewport, touch, errors }
  try {
    await page.goto(new URL("/?fixture=" + id, origin).href, { waitUntil: "networkidle" })
    await page.evaluate(() => document.fonts.ready)
    await page.locator("#root > *").first().waitFor({ state: "attached" })
    assert.equal(await page.evaluate(() => matchMedia("(pointer:coarse)").matches), touch)
    const rendered = await page.evaluate(() => {
      const visible = elem => !!elem.getClientRects().length
      const rect = elem => {
        const { x, y, width, height } = elem.getBoundingClientRect()
        return { x, y, width, height }
      }
      const inspect = selector => Array.from(document.querySelectorAll(selector)).filter(visible)
        .map(elem => ({ name: elem.getAttribute("aria-label") || elem.innerText?.trim().slice(0, 80), ...rect(elem) }))
      return { route: location.hash, pageWidth: document.documentElement.scrollWidth,
        headings: inspect("h1,h2,h3"), dialogs: inspect('[role="dialog"]'),
        navigation: inspect("aside,nav"), text: document.body.innerText.trim(),
        fonts: Array.from(document.fonts, font => ({ family: font.family, status: font.status })),
      }
    })
    Object.assign(record, rendered)
    assert.ok(rendered.text || await page.locator("canvas").count(), "Empty scene")
    assert.doesNotMatch(rendered.text, /Connecting to platform|Username\s+Password\s+Sign in/)
    await page.screenshot({ path: path.join(output, key + ".png"), animations: "disabled" })
    if (rendered.pageWidth > viewport.width + 1) errors.push("Horizontal overflow: " + rendered.pageWidth)
    if (id === "02") {
      await checkDrawer(page)
      await page.goto(new URL("/?fixture=02", origin).href)
      await checkPalette(page)
      record.checked = ["drawer width, nested Esc, list filter, tab deep link", "palette focus, navigation, no results and activation"]
    }
    if (id === "36") {
      const dialog = page.getByRole("dialog", { name: "New machine" })
      assert.equal(await dialog.locator('[aria-invalid="true"]').count(), 1)
      assert.ok(await dialog.getByRole("button", { name: "Create", exact: true }).isDisabled())
      record.checked = ["invalid name blocks submission"]
    }
    if (id === "46") {
      assert.equal(await page.getByRole("button", { name: "Open navigation" }).isVisible(), touch)
      assert.equal(await page.locator("aside.sidebar").isVisible(), !touch)
      record.checked = [touch ? "touch navigation" : "desktop navigation"]
    }
  } catch (error) {
    errors.push(error.message)
  } finally {
    await context.close()
  }
  results.push(record)
  console.log(key, record.route || "", errors.length ? errors.join(" | ") : "captured")
}

try {
  for (const file of files) {
    if (selected.length && !selected.includes(file.slice(0, 2))) continue
    await capture(file)
    if (file.startsWith("46-")) await capture(file, true)
  }
  assert.ok(results.length, "No matching scenes")
  await writeFile(path.join(output, "observations.json"), JSON.stringify({ browser: browser.version(), results }, null, 2))
  if (results.some(record => record.errors.length)) process.exitCode = 1
} finally {
  await browser.close()
}
