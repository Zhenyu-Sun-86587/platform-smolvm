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
