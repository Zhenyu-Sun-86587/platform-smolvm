// The SDK uses a fake fetch and the console uses fake /v1 responses on a local preview.
import assert from "node:assert/strict"
import { Client, PlatformError } from "../../sdk/typescript/dist/index.js"

const profile = "kernel_cow_anon_v1"
const native = { status: "supported", supported: true, contract_version: 1, verified_profile: "managed-parked-v1" }
const inventory = {
  kernel: { ...native, memory_fork: { profile, native_supported: false, kernel_cow_supported: true } },
  native: { ...native, memory_fork: {
    profile: "native", native_supported: true, kernel_cow_supported: false,
    unavailable_reason: "MEMORY_FORK_BACKEND_UNAVAILABLE",
  } },
  old: native,
}
const verified = (profile) => ({ status: "supported", supported: true, contract_version: 1, verified_profile: profile })
const runtimes = [
  {
    runtime_instance_id: "kernel-named-stock-worker", capabilities: {
      "boot.system": verified("system-debian-v1"),
      "boot.kernel_cow_ram": { status: "unknown", supported: false, reason_code: "KERNEL_COW_PROFILE_UNVERIFIED" },
    },
  },
  {
    runtime_instance_id: "verified-worker", capabilities: {
      "boot.system": verified("system-debian-v1"),
      "boot.kernel_cow_ram": verified(profile),
      "boot.live_compute_growth": verified("managed-live-compute-v1"),
    },
  },
]
const denied = {
  error: {
    code: "CAPABILITY_UNSUPPORTED", capability: "state.memory_fork", reason_code: "MEMORY_FORK_BACKEND_UNAVAILABLE",
    message: "state.memory_fork is unsupported for this runtime", effect: "none", retry: "after_precondition",
  },
}
const queued = (id, kind) => ({ operation_id: id, kind, state: "queued", revision: 1, effect: "none", stage: "queued" })
const complete = (id, kind, fields) => ({ ...queued(id, kind), state: "succeeded", revision: 2, effect: "applied", stage: "complete", ...fields })
const forkResult = (requested, selected) => ({
  requested_memory_fork_backend: requested,
  selected_memory_fork_backend: selected,
  memory_fork_profile: selected === "kernel_cow" ? profile : "native",
})
const optionDisabled = (select, value) => select.locator('option[value="' + value + '"]').evaluate(option => option.disabled)

async function checkSDK() {
  const posted = []
  const client = new Client("http://localhost", "fixture")
  const options = { intent: { key: "intent", epoch: "epoch" }, revision: 7, generation: 3 }
  const terminal = complete("op_sdk", "independent_fork", { result: forkResult("auto", "kernel_cow") })
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (url, request) => {
    if (request.method === "POST") {
      assert.equal(request.headers.get("Authorization"), "Bearer fixture")
      assert.equal(request.headers.get("If-Control-Epoch"), "epoch")
      assert.equal(request.headers.get("If-Match"), '"7"')
      const body = JSON.parse(request.body)
      posted.push([url.pathname, body])
      return body.child_name === "denied" ? Response.json(denied, { status: 409 })
        : Response.json(queued("op_sdk", "independent_fork"), { status: 202 })
    }
    if (url.pathname === "/v1/runtime-profiles") return Response.json({ items: runtimes })
    if (url.pathname === "/v1/machines/m_source") {
      return Response.json({ capabilities: { "state.independent_fork": inventory.kernel }, memory_fork_profile: profile })
    }
    assert.equal(url.pathname, "/v1/operations/op_sdk")
    return Response.json(terminal)
  }
  try {
    const firstPage = await client.listRuntimeProfiles().next()
    assert.equal(firstPage.value.capabilities["boot.kernel_cow_ram"].status, "unknown")
    const machine = await client.getMachine("m_source")
    assert.equal(machine.memory_fork_profile, profile)
    assert.deepEqual(machine.capabilities["state.independent_fork"], inventory.kernel)
    const spec = { name: "source", template_id: "system", runtime_instance_id: "verified-worker", memory_fork_profile: profile }
    await client.createMachine(spec, options)
    await client.fork("m_source", "legacy", options)
    await assert.rejects(client.fork("m_source", "invalid", { ...options, memoryForkBackend: "kernel_cow" }), /requires requireIndependent/)
    const accepted = await client.fork("m_source", "child", { ...options, requireIndependent: true, memoryForkBackend: "auto" })
    assert.deepEqual(await client.waitOperation(accepted.operation_id), terminal)
    await client.forkBatch("m_source", { children: [{ name: "batch" }], memory_fork_backend: "kernel_cow" }, options)
    await assert.rejects(client.fork("m_source", "denied", { ...options, requireIndependent: true, memoryForkBackend: "native" }),
      error => error instanceof PlatformError && error.detail.capability === "state.memory_fork" &&
        error.detail.reason_code === "MEMORY_FORK_BACKEND_UNAVAILABLE" && error.detail.effect === "none")
    assert.deepEqual(posted, [
      ["/v1/machines", spec],
      ["/v1/machines/m_source/forks", { child_name: "legacy", expected_generation: 3 }],
      ["/v1/machines/m_source/forks", { child_name: "child", expected_generation: 3, require_independent: true, memory_fork_backend: "auto" }],
      ["/v1/machines/m_source/fork-batches", { children: [{ name: "batch" }], expected_generation: 3, require_independent: true, memory_fork_backend: "kernel_cow" }],
      ["/v1/machines/m_source/forks", { child_name: "denied", expected_generation: 3, require_independent: true, memory_fork_backend: "native" }],
    ])
  } finally {
    globalThis.fetch = originalFetch
  }
  console.log("TypeScript SDK memory Fork contract passed")
}

function sourceMachine(kind, capability) {
  return {
    machine_id: "m_" + kind, name: kind + " source", template_id: "system", boot_mode: "system",
    revision: 7, binding_generation: 3, power_state: "running", desired_power: "running",
    observed_state: "running", availability: "available", binding: { runtime_bundle_id: "test-bundle" },
    capabilities: { "state.independent_fork": capability, "state.fork_batch": capability },
  }
}

async function checkConsole(origin) {
  const { chromium } = await import(process.env.PLAYWRIGHT_CORE || "playwright-core")
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM })
  try {
    const page = await browser.newPage()
    page.setDefaultTimeout(8000)
    const errors = []
    page.on("pageerror", error => errors.push(error.message))
    const machines = new Map(Object.entries(inventory).map(([kind, capability]) => ["m_" + kind, sourceMachine(kind, capability)]))
    machines.get("m_kernel").power_state = "standby"
    const operations = new Map()
    const posted = []
    let rejectFork = false
    let emptyGraph = false
    await page.route("**/v1/**", async route => {
      const request = route.request()
      const [group, id, action] = new URL(request.url()).pathname.split("/").slice(2)
      if (request.method() === "POST") {
        assert.equal(request.headers()["x-csrf-token"], "csrf")
        const body = request.postDataJSON()
        posted.push(body)
        if (rejectFork) return route.fulfill({ status: 409, json: denied })
        const operation = "op_" + posted.length
        let kind = "create"
        let fields
        if (action) {
          assert.ok(action === "forks" || action === "fork-batches", action)
          kind = action === "forks" ? "independent_fork" : "fork_batch"
          const result = forkResult(body.memory_fork_backend ?? "native", id === "m_kernel" ? "kernel_cow" : "native")
          if (id === "m_old") delete result.memory_fork_profile
          fields = { result }
          assert.equal(request.headers()["if-match"], '"7"')
        } else {
          const machineID = "m_created_" + posted.length
          machines.set(machineID, { ...sourceMachine(machineID, inventory.old), ...body, machine_id: machineID })
          fields = { machine_id: machineID }
        }
        operations.set(operation, complete(operation, kind, fields))
        return route.fulfill({ status: 202, json: queued(operation, kind) })
      }
      if (group === "control") return route.fulfill({ json: {
        control_epoch: "epoch", csrf_token: "csrf", default_boot_mode: "system", runtime_ready: true,
      } })
      if (group === "events") return route.fulfill({ contentType: "text/event-stream", body: ": fixture\n\n" })
      if (group === "templates") return route.fulfill({ json: { items: [
        { template_id: "system", version: 1, name: "System", spec: { boot_mode: "system" } },
        { template_id: "oci", version: 1, name: "OCI", spec: { boot_mode: "oci" } },
      ] } })
      if (group === "runtime-profiles") return route.fulfill({ json: { items: runtimes } })
      if (group === "operations" && id) return route.fulfill({ json: operations.get(id) })
      if (group === "machines" && !id) return route.fulfill({ json: { items: [...machines.values()] } })
      if (group === "machines" && !action) return route.fulfill({ json: machines.get(id) })
      if (group === "machines" && action === "dependencies") {
        const items = !emptyGraph && machines.has(id)
          ? [{ parent_machine_id: id, child_machine_id: "m_linked", kind: "logical_origin", status: "active" }] : []
        return route.fulfill({ json: { items } })
      }
      return route.fulfill({ json: { items: [] } })
    })

    const newMachine = async () => {
      await page.getByRole("button", { name: "New machine", exact: true }).first().click()
      const dialog = page.getByRole("dialog", { name: "New machine" })
      await dialog.getByLabel("Runtime", { exact: true }).waitFor()
      await dialog.getByText("Advanced options", { exact: true }).click()
      return dialog
    }
    await page.goto(origin + "/#/machines")
    let dialog = await newMachine()
    let ram = dialog.getByLabel("RAM profile", { exact: true })
    await dialog.getByText("KERNEL_COW_PROFILE_UNVERIFIED", { exact: true }).waitFor()
    assert.equal(await optionDisabled(ram, profile), true)
    await dialog.getByRole("button", { name: "Create", exact: true }).click()
    await dialog.getByRole("button", { name: "Done" }).click()
    assert.equal("memory_fork_profile" in posted[0], false)

    dialog = await newMachine()
    ram = dialog.getByLabel("RAM profile", { exact: true })
    const runtime = dialog.getByLabel("Runtime", { exact: true })
    await runtime.selectOption("verified-worker")
    const growth = dialog.getByRole("checkbox", { name: /Enable live CPU and RAM growth/ })
    await growth.check()
    assert.equal(await optionDisabled(ram, profile), true)
    await growth.uncheck()
    await ram.selectOption(profile)
    assert.equal(await growth.isDisabled(), true)
    const create = dialog.getByRole("button", { name: "Create", exact: true })
    await runtime.selectOption("kernel-named-stock-worker")
    assert.equal(await create.isDisabled(), true)
    assert.equal(await ram.inputValue(), profile)
    await runtime.selectOption("verified-worker")
    await create.click()
    await dialog.getByText("Configured RAM profile · Kernel CoW (fixed anonymous RAM)", { exact: true }).waitFor()
    assert.equal(posted[1].memory_fork_profile, profile)
    assert.equal(posted[1].runtime_instance_id, "verified-worker")
    assert.equal("live_compute_growth" in posted[1], false)
    await dialog.getByRole("button", { name: "Done" }).click()

    for (const id of ["m_old", "m_native", "m_kernel"]) {
      await page.goto(origin + "/#/machines/" + id + "/forks")
      await page.getByRole("heading", { name: id.slice(2) + " source", exact: true }).first().waitFor()
      await page.getByRole("button", { name: "Independent Fork", exact: true }).click()
      dialog = page.getByRole("dialog", { name: "Independent Fork" })
      const backend = dialog.locator('select[name="memory_fork_backend"]')
      const submit = dialog.getByRole("button", { name: "Create fork", exact: true })
      if (id === "m_kernel") {
        assert.equal(await backend.evaluate(select => select.checkValidity()), false)
        assert.equal(await optionDisabled(backend, "native"), true)
        await backend.selectOption("kernel_cow")
        rejectFork = true
        await submit.click()
        await dialog.getByText("MEMORY_FORK_BACKEND_UNAVAILABLE", { exact: true }).waitFor()
        assert.equal(await backend.inputValue(), "kernel_cow")
        rejectFork = false
      } else {
        assert.equal(await optionDisabled(backend, "kernel_cow"), true)
        assert.equal(await optionDisabled(backend, "auto"), false)
        assert.equal(await backend.inputValue(), "")
      }
      await submit.click()
      await dialog.waitFor({ state: "hidden" })
    }
    assert.deepEqual(posted.slice(2).map(body => body.memory_fork_backend), [undefined, undefined, "kernel_cow", "kernel_cow"])
    assert.ok(posted.slice(2).every(body => body.require_independent && body.expected_generation === 3))
    await page.getByText("Memory backend · Kernel CoW", { exact: true }).waitFor()

    await page.getByRole("button", { name: "Fork batch", exact: true }).click()
    dialog = page.getByRole("dialog", { name: "Fork batch" })
    await dialog.getByLabel("Child names, one per line").fill("one\ntwo")
    await dialog.locator('select[name="memory_fork_backend"]').selectOption("auto")
    await dialog.getByRole("button", { name: "Fork batch", exact: true }).click()
    await dialog.waitFor({ state: "hidden" })
    assert.deepEqual(posted.at(-1), {
      children: [{ name: "one" }, { name: "two" }], expected_generation: 3, require_independent: true, memory_fork_backend: "auto",
    })
    await page.goto(origin + "/#/operations/op_" + posted.length)
    await page.locator("dl div").filter({ hasText: "Requested memory fork backend" }).getByText("Auto", { exact: true }).waitFor()
    await page.locator("dl div").filter({ hasText: "Selected memory fork backend" }).getByText("Kernel CoW", { exact: true }).waitFor()
    emptyGraph = true
    await page.goto(origin + "/#/machines/m_kernel/forks")
    await page.getByText("sandboxctl machines live-fork m_kernel child-name --memory-fork-backend kernel_cow", { exact: true }).waitFor()
    assert.deepEqual(errors, [])
    console.log("Console create preflight, old default, single Fork and batch receipts passed")
  } finally {
    await browser.close()
  }
}

await checkSDK()
if (process.argv[2]) await checkConsole(process.argv[2])
