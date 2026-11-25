// Run with SANDBOX_URL, SANDBOX_TOKEN, SANDBOX_TEMPLATE, and SANDBOX_NAME set.
import { Client } from "../sdk/typescript/dist/index.js"

for (const key of ["SANDBOX_URL", "SANDBOX_TOKEN", "SANDBOX_TEMPLATE", "SANDBOX_NAME"]) {
  if (!process.env[key]) throw new Error(`Set ${key}`)
}
const { SANDBOX_URL: url, SANDBOX_TOKEN: token, SANDBOX_TEMPLATE: template, SANDBOX_NAME: name } = process.env
let client = new Client(url, token)

async function success(accepted) {
  console.error(`Observing ${accepted.operation_id}`)
  const result = await client.waitOperation(accepted.operation_id)
  if (result.state !== "succeeded") throw new Error(JSON.stringify(result))
  return result.result
}

const { machine_id: machine } = await success(await client.createMachine({
  name, template_id: template, start: true,
  resources: { cpu_count: 1, memory_mib: 1024, storage_gib: 4, overlay_gib: 4 },
  idle_policy: { enabled: false, timeout_seconds: 1800 }
}))
const { task_id: task } = await success(await client.startTask(machine, {
  argv: ["/bin/sh", "-c", "for n in 1 2 3; do printf 'step %s\\n' \"$n\"; sleep 2; done"]
}))
console.error(`Persisted ${machine} / ${task}`)

let offset = 0
const decoder = new TextDecoder()
function write(chunk) {
  offset = chunk.next
  if (chunk.gap) console.error(`Output gap before byte ${chunk.start}`)
  process.stdout.write(decoder.decode(chunk.bytes, { stream: true }))
}
for await (const chunk of client.taskOutput(task)) {
  write(chunk)
  break
}
client = new Client(url, token)
for await (const chunk of client.taskOutput(task, "stdout", offset)) write(chunk)
process.stdout.write(decoder.decode())
const status = await client.getTask(task)
if (status.exit_code !== 0) throw new Error(JSON.stringify(status))
const captured = await success(await client.capture(machine, "clone", "After task"))
console.log(`Snapshot ${captured.snapshot_id}`)
const cloned = await success(await client.clone(machine, name + "-clone"))
console.log(`Clone ${cloned.machine_id}`)

if (process.env.SANDBOX_EXAMPLE_SERVICE === "1") {
  const app = await success(await client.startTask(cloned.machine_id, {
    argv: ["/usr/bin/python3", "-m", "http.server", "8080", "-d", "/workspace"]
  }))
  try {
    const published = await success(await client.expose(cloned.machine_id, {
      name: "workspace-http", protocol: "http", guest_port: 8080
    }))
    const endpoint = await client.get("services/" + published.service_id)
    const url = "https://" + endpoint.public_host + "/"
    const response = await fetch(url, { headers: { Authorization: "Bearer " + token }, redirect: "error" })
    if (!response.ok) throw new Error(`Service returned HTTP ${response.status}`)
    console.log(`Service ${url} returned HTTP ${response.status}`)
    await response.body?.cancel()
  } finally {
    await success(await client.cancelTask(app.task_id, 0))
  }
}
