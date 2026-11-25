// Build the source offer from an allowlist. Never archive local state or credentials.
import { execFileSync } from "node:child_process"
import { readdirSync, readFileSync, writeFileSync, existsSync } from "node:fs"
import { resolve, join } from "node:path"
const web = resolve(import.meta.dirname, "..")
const root = resolve(web, "..")
const dest = join(web, "dist/assets")
const lock = JSON.parse(readFileSync(join(web, "package-lock.json")))
const parts = [readFileSync(join(web, "licenses/README.md"), "utf8")]
for (const f of ["DAYTONA-COPYRIGHT", "DAYTONA-NOTICE", "DAYTONA-LICENSE"])
  parts.push(readFileSync(join(web, "licenses", f), "utf8"))
for (const [dir, meta] of Object.entries(lock.packages)) {
  if (!dir || meta.dev || !existsSync(join(web, dir))) continue
  const licenseFiles = readdirSync(join(web, dir), { withFileTypes: true }).filter(
    (f) => f.isFile() && /^(licen[cs]e|copying|notice|ofl)(?:[.-]|$)/i.test(f.name),
  )
  parts.push(
    dir.replace(/^node_modules\//, "") + " " + meta.version + " (" + meta.license + ")",
    ...licenseFiles.map((f) => readFileSync(join(web, dir, f.name), "utf8")),
  )
}
writeFileSync(join(dest, "console-notices.txt"), parts.join("\n\n"))
execFileSync(
  "tar",
  [
    "--sort=name",
    "--mtime=@0",
    "--numeric-owner",
    "--owner=0",
    "--group=0",
    "--exclude=node_modules",
    "--exclude=dist",
    "--exclude=__pycache__",
    "--exclude=*.egg-info",
    "-czf",
    join(dest, "console-source.tar.gz"),
    "web/src",
    "web/index.html",
    "web/tsconfig.json",
    "web/package.json",
    "web/package-lock.json",
    "web/postcss.config.mjs",
    "web/scripts",
    "web/licenses",
    "web/handler.go",
    "sdk/typescript/src",
    "api",
    "cmd",
    "internal",
    "migrations",
    "guest",
    "packaging",
    "go.mod",
    "go.sum",
  ],
  { cwd: root },
)
