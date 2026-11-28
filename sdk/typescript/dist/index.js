export const terminalStates = new Set(["succeeded", "failed", "cancelled", "recovery_required"]);
export class PlatformError extends Error {
    status;
    detail;
    constructor(status, detail) {
        super(detail.message ?? `HTTP ${status}`);
        this.status = status;
        this.detail = detail;
        this.name = "PlatformError";
    }
}
const segment = encodeURIComponent;
export function machine(id) { return "machines/" + segment(id); }
function packetWakeRule(id, source, transport, port) {
    return machine(id) + "/network/wake-rules/" + segment(source) + "/" + transport + "/" + port;
}
export function task(id) { return "tasks/" + segment(id); }
export function snapshot(id) { return "snapshots/" + segment(id); }
export function volume(id) { return "volumes/" + segment(id); }
export function volumeSnapshot(id) { return "volume-snapshots/" + segment(id); }
export function service(id) { return "services/" + segment(id); }
export function operation(id) { return "operations/" + segment(id); }
function pause(ms, signal) {
    return new Promise((resolve, reject) => {
        signal?.throwIfAborted();
        const timer = setTimeout(() => {
            signal?.removeEventListener("abort", abort);
            resolve();
        }, ms);
        const abort = () => {
            clearTimeout(timer);
            reject(signal?.reason);
        };
        signal?.addEventListener("abort", abort, { once: true });
    });
}
function withQuery(path, params) {
    return path + (path.includes("?") ? "&" : "?") + new URLSearchParams(params);
}
export class Client {
    token;
    base;
    csrf;
    constructor(url, token) {
        this.token = token;
        this.base = new URL(url.replace(/\/$/, "") + "/v1/");
    }
    url(path) {
        const pathname = path.split("?")[0];
        if (!pathname || pathname.startsWith("/") || /[:\\]/.test(pathname) ||
            pathname.split("/").some(part => [".", ".."].includes(decodeURIComponent(part)))) {
            throw new Error("Expected a relative API path");
        }
        const url = new URL(path, this.base);
        if (url.origin !== this.base.origin || !url.pathname.startsWith(this.base.pathname)) {
            throw new Error("Expected a relative API path");
        }
        return url;
    }
    async response(path, init = {}) {
        const headers = new Headers(init.headers);
        if (this.token)
            headers.set("Authorization", `Bearer ${this.token}`);
        if (this.csrf && !["GET", "HEAD"].includes(init.method ?? "GET")) {
            headers.set("X-CSRF-Token", this.csrf);
        }
        const response = await fetch(this.url(path), { ...init, headers, redirect: "error", credentials: "same-origin" });
        if (!response.ok) {
            const doc = await response.json().catch(() => ({}));
            throw new PlatformError(response.status, doc?.error ?? {});
        }
        return response;
    }
    async json(path, init) {
        const response = await this.response(path, init);
        if (response.status === 204)
            return undefined;
        return response.json();
    }
    get(path, signal) { return this.json(path, { signal }); }
    async control(signal) {
        const control = await this.get("control", signal);
        this.csrf = control.csrf_token;
        return control;
    }
    async login(username, password) {
        const login = await this.json("auth/login", {
            method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username, password })
        });
        this.csrf = login.csrf_token;
        return login;
    }
    async logout() {
        await this.response("auth/logout", { method: "POST" });
        this.csrf = undefined;
    }
    async intent(signal) {
        const control = await this.control(signal);
        return { epoch: control.control_epoch, key: crypto.randomUUID() };
    }
    async writeHeaders(options) {
        const intent = options.intent ?? await this.intent(options.signal);
        const headers = new Headers({ "If-Control-Epoch": intent.epoch, "Idempotency-Key": intent.key });
        if (options.revision !== undefined)
            headers.set("If-Match", `"${options.revision}"`);
        return headers;
    }
    async mutate(path, body = {}, options = {}) {
        const headers = await this.writeHeaders(options);
        headers.set("Content-Type", "application/json");
        return this.json(path, {
            method: options.method ?? "POST", headers, body: JSON.stringify(body), signal: options.signal
        });
    }
    async upload(path, blob, options = {}) {
        const headers = await this.writeHeaders(options);
        headers.set("Content-Type", "application/octet-stream");
        if (options.sha256)
            headers.set("X-Content-SHA256", options.sha256);
        return this.json(path, { method: options.method ?? "PUT", headers, body: blob, signal: options.signal });
    }
    async waitOperation(id, { timeoutMs = 180000, signal, onUpdate } = {}) {
        const timeout = AbortSignal.timeout(timeoutMs);
        const observe = signal ? AbortSignal.any([signal, timeout]) : timeout;
        try {
            for (;;) {
                const op = await this.get(operation(id), observe);
                onUpdate?.(op);
                if (terminalStates.has(op.state))
                    return op;
                await pause(350, observe);
            }
        }
        catch (error) {
            if (timeout.aborted && !signal?.aborted)
                throw new Error(`Timed out observing operation ${id}`);
            throw error;
        }
    }
    wait(id, timeoutMs) { return this.waitOperation(id, { timeoutMs }); }
    async *pages(path, limit = 100, signal) {
        let cursor = "";
        do {
            const page = await this.get(withQuery(path, { limit: String(limit), cursor }), signal);
            yield* page.items;
            if (page.next_cursor && page.next_cursor === cursor)
                throw new Error("Server did not advance cursor");
            cursor = page.next_cursor ?? "";
        } while (cursor);
    }
    async *taskOutput(id, stream = "stdout", offset = 0, signal) {
        const path = task(id) + "/output";
        for (;;) {
            const chunk = await this.get(withQuery(path, { stream, offset: String(offset), limit: "65536" }), signal);
            const bytes = Uint8Array.from(atob(chunk.data_base64 || ""), byte => byte.charCodeAt(0));
            if (chunk.next < offset || (bytes.length && chunk.next === offset))
                throw new Error("Invalid task output cursor");
            if (bytes.length || chunk.gap || chunk.next !== offset) {
                yield { start: chunk.start, next: chunk.next, gap: chunk.gap, bytes };
            }
            offset = chunk.next;
            if (chunk.complete)
                return;
            if (!bytes.length)
                await pause(400, signal);
        }
    }
    async *events(lastEventId = "", signal) {
        const headers = new Headers({ Accept: "text/event-stream" });
        if (lastEventId)
            headers.set("Last-Event-ID", lastEventId);
        const response = await this.response("events", { headers, signal });
        if (!response.body)
            throw new Error("Event stream has no body");
        const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
        let pending = "";
        let id = lastEventId;
        let type = "message";
        let data = [];
        try {
            for (;;) {
                const { value, done } = await reader.read();
                if (done)
                    return;
                pending += value;
                let newline = pending.indexOf("\n");
                while (newline >= 0) {
                    const line = pending.slice(0, newline).replace(/\r$/, "");
                    pending = pending.slice(newline + 1);
                    if (!line) {
                        if (data.length) {
                            const text = data.join("\n");
                            yield { id, type, data: JSON.parse(text) };
                        }
                        type = "message";
                        data = [];
                    }
                    else {
                        const colon = line.indexOf(":");
                        const field = colon < 0 ? line : line.slice(0, colon);
                        const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
                        if (field === "id" && !value.includes("\0"))
                            id = value;
                        if (field === "event")
                            type = value;
                        if (field === "data")
                            data.push(value);
                    }
                    newline = pending.indexOf("\n");
                }
            }
        }
        finally {
            await reader.cancel().catch(() => { });
            reader.releaseLock();
        }
    }
    async browserSocket(path, ticketPath, protocol, signal) {
        const ticket = await this.json(ticketPath, { method: "POST", signal });
        signal?.throwIfAborted();
        const url = this.url(path);
        url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
        return new WebSocket(url, [protocol, "ticket." + ticket.ticket]);
    }
    socket(path, protocol, connect) {
        if (!this.token)
            throw new Error("Bearer authentication is required for a header-based socket");
        const url = this.url(path);
        url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
        return connect(url, protocol, new Headers({ Authorization: `Bearer ${this.token}` }));
    }
    createMachine(spec, options) { return this.mutate("machines", spec, options); }
    getMachine(id, signal) { return this.get(machine(id), signal); }
    getMachineMetrics(id, signal) { return this.get(machine(id) + "/metrics", signal); }
    getSystemMetrics(signal) { return this.get("system/metrics", signal); }
    listFiles(id, path, signal) {
        const endpoint = machine(id) + "/files/list";
        return this.pages(path === undefined ? endpoint : withQuery(endpoint, { path }), 100, signal);
    }
    downloadFile(id, path, signal) {
        return this.response(withQuery(machine(id) + "/files", { path }), { signal });
    }
    uploadFile(id, path, data, options = {}) {
        return this.upload(withQuery(machine(id) + "/files", { path }), data, options);
    }
    getMachineResources(id, signal) { return this.get(machine(id) + "/resources", signal); }
    async configureResources(id, resources, options = {}) {
        let { revision, generation } = options;
        if (revision === undefined || generation === undefined) {
            const current = await this.getMachine(id, options.signal);
            revision ??= current.revision;
            generation ??= current.binding_generation;
        }
        return this.mutate(machine(id), { resources, expected_generation: generation }, { ...options, method: "PATCH", revision });
    }
    listPrivateNetworks(signal) { return this.pages("networks", 100, signal); }
    getMachineNetwork(id, signal) { return this.get(machine(id) + "/network", signal); }
    async setNetworkDefaults(defaults, options = {}) {
        const revision = options.revision ?? (await this.get("system/settings", options.signal)).revision;
        return this.mutate("system/settings", defaults, { ...options, method: "PATCH", revision });
    }
    getLifecycleStatus(id, signal) {
        return this.get(machine(id) + "/lifecycle", signal);
    }
    async setDefaultLifecyclePolicy(policy, options = {}) {
        const revision = options.revision ?? (await this.get("system/settings", options.signal)).revision;
        return this.mutate("system/settings", { default_lifecycle_policy: policy }, { ...options, method: "PATCH", revision });
    }
    async updateLifecyclePolicy(id, policy, options = {}) {
        const current = await this.getMachine(id, options.signal);
        const policyRevision = options.policyRevision ?? current.lifecycle_policy?.desired_revision;
        if (policyRevision === undefined)
            throw new Error("Machine has no lifecycle policy revision");
        return this.mutate(machine(id), { lifecycle_policy: policy, expected_policy_revision: policyRevision,
            expected_generation: options.generation ?? current.binding_generation }, { ...options, method: "PATCH", revision: options.revision ?? current.revision });
    }
    applyDefaultLifecyclePolicy(revision, machines, options) {
        return this.mutate("lifecycle-policy-applications", { default_policy_revision: revision, machines }, options);
    }
    listRunningLeases(id, signal) {
        return this.pages(machine(id) + "/leases", 100, signal);
    }
    async acquireRunningLease(id, reason, ttlSeconds, options = {}) {
        const revision = options.revision ?? (await this.getMachine(id, options.signal)).revision;
        return this.mutate(machine(id) + "/leases", { semantics_version: 2, reason, ttl_seconds: ttlSeconds }, { ...options, revision });
    }
    async renewRunningLease(lease, ttlSeconds, options = {}) {
        if (!lease.lease_token)
            throw new Error("A current running lease token is required");
        const next = await this.mutate("leases/" + segment(lease.lease_id), { lease_token: lease.lease_token, ttl_seconds: ttlSeconds }, { ...options, revision: lease.revision, method: "PATCH" });
        return { ...next, lease_token: lease.lease_token };
    }
    releaseRunningLease(lease, options = {}) {
        if (!lease.lease_token)
            throw new Error("A current running lease token is required");
        return this.mutate("leases/" + segment(lease.lease_id), { lease_token: lease.lease_token }, { ...options, revision: lease.revision, method: "DELETE" });
    }
    async withActivity(id, reason, run, ttlSeconds = 120) {
        let lease = await this.acquireRunningLease(id, reason, ttlSeconds);
        const progress = new AbortController();
        let timer;
        let renewing = Promise.resolve();
        let closed = false;
        const schedule = () => {
            timer = setTimeout(() => {
                renewing = this.renewRunningLease(lease, ttlSeconds).then(next => {
                    lease = next;
                    if (!closed)
                        schedule();
                }).catch(error => progress.abort(error));
            }, ttlSeconds * 1000 / 3);
        };
        schedule();
        let finished = false;
        try {
            const result = await run(progress.signal);
            progress.signal.throwIfAborted();
            finished = true;
            return result;
        }
        finally {
            closed = true;
            clearTimeout(timer);
            await renewing;
            try {
                await this.releaseRunningLease(lease);
            }
            catch (error) {
                if (finished)
                    throw error;
            }
            if (finished)
                progress.signal.throwIfAborted();
        }
    }
    getPrivatePacketWakeRule(id, source, transport, port, signal) {
        return this.get(packetWakeRule(id, source, transport, port), signal);
    }
    async setPrivatePacketWakeRule(id, source, transport, port, enabled, ruleRevision, options = {}) {
        const current = options.revision === undefined || options.generation === undefined ? await this.getMachine(id, options.signal) : undefined;
        return this.mutate(packetWakeRule(id, source, transport, port), {
            enabled, expected_revision: ruleRevision, expected_generation: options.generation ?? current?.binding_generation,
        }, { ...options, revision: options.revision ?? current?.revision, method: "PUT" });
    }
    getOperation(id, signal) { return this.get(operation(id), signal); }
    listBackupPolicies(signal) { return this.pages("backups/policies", 100, signal); }
    listBackups(signal) { return this.pages("backups", 100, signal); }
    getBackup(id, signal) { return this.get("backups/" + segment(id), signal); }
    runBackup(policy, options) {
        return this.mutate("backups/policies/" + segment(policy) + "/runs", {}, options);
    }
    async retryForkTargets(id, options = {}) {
        const revision = options.revision ?? (await this.getOperation(id, options.signal)).revision;
        return this.mutate(operation(id) + "/retry", {}, { ...options, revision });
    }
    cancelOperation(id, options = {}) {
        return this.mutate(operation(id) + "/cancel", {}, options);
    }
    listServices(id, signal) {
        return this.pages(machine(id) + "/services", 100, signal);
    }
    getService(id, signal) { return this.get(service(id), signal); }
    listDesktopSessions(id, signal) {
        return this.pages(machine(id) + "/desktop-sessions", 100, signal);
    }
    async createDesktopSession(id, serviceId, ttlSeconds, options = {}) {
        const current = options.generation === undefined || options.revision === undefined ? await this.getMachine(id, options.signal) : undefined;
        return this.mutate(machine(id) + "/desktop-sessions", {
            service_id: serviceId, expected_generation: options.generation ?? current?.binding_generation, ttl_seconds: ttlSeconds,
        }, { ...options, revision: options.revision ?? current?.revision });
    }
    closeDesktopSession(id, options) {
        return this.mutate("desktop-sessions/" + segment(id), {}, { ...options, method: "DELETE" });
    }
    desktopSocket(id, signal) {
        const path = "desktop-sessions/" + segment(id);
        return this.browserSocket(path + "/tunnel", path + "/tunnel-tickets", "sandbox.rfb.v1", signal);
    }
    listSnapshots(id, signal) {
        return this.pages(id ? machine(id) + "/snapshots" : "snapshots", 100, signal);
    }
    getSnapshot(id, signal) { return this.get(snapshot(id), signal); }
    listMachines(signal) { return this.pages("machines", 100, signal); }
    listVolumes(signal) { return this.pages("volumes", 100, signal); }
    createVolume(spec, options) { return this.mutate("volumes", spec, options); }
    getVolume(id, signal) { return this.get(volume(id), signal); }
    async growVolume(id, capacityBytes, options = {}) {
        const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision;
        return this.mutate(volume(id), { capacity_bytes: capacityBytes }, { ...options, method: "PATCH", revision });
    }
    async deleteVolume(id, options = {}) {
        const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision;
        return this.mutate(volume(id), {}, { ...options, method: "DELETE", revision });
    }
    listVolumeSnapshots(id, signal) {
        return this.pages(volume(id) + "/snapshots", 100, signal);
    }
    getVolumeSnapshot(id, signal) {
        return this.get(volumeSnapshot(id), signal);
    }
    async createVolumeSnapshot(id, options = {}) {
        const revision = options.revision ?? (await this.getVolume(id, options.signal)).revision;
        return this.mutate(volume(id) + "/snapshots", {}, { ...options, revision });
    }
    async deleteVolumeSnapshot(id, options = {}) {
        const revision = options.revision ?? (await this.getVolumeSnapshot(id, options.signal)).volume_revision;
        return this.mutate(volumeSnapshot(id), {}, { ...options, method: "DELETE", revision });
    }
    cloneVolumeSnapshot(id, name, options) {
        return this.mutate(volumeSnapshot(id) + "/clones", { name }, options);
    }
    listVolumeAttachments(id, signal) {
        return this.pages(machine(id) + "/volume-attachments", 100, signal);
    }
    async attachVolume(id, spec, options = {}) {
        const expectedVolumeRevision = spec.expected_volume_revision ?? (await this.getVolume(spec.volume_id, options.signal)).revision;
        return this.machineMutation(id, "volume-attachments", { ...spec, expected_volume_revision: expectedVolumeRevision }, options);
    }
    detachVolume(id, volumeRevision, options) {
        return this.mutate("volume-attachments/" + segment(id), { expected_volume_revision: volumeRevision, expected_generation: options.generation }, { ...options, method: "DELETE" });
    }
    listWorkers(signal) { return this.pages("workers", 100, signal); }
    getWorker(id, signal) { return this.get("workers/" + encodeURIComponent(id), signal); }
    getPlacement(operationId, signal) { return this.get(operation(operationId) + "/placement", signal); }
    setWorkerMode(id, mode, reason, revision, options) {
        return this.mutate("workers/" + encodeURIComponent(id) + "/maintenance", { mode, reason }, { ...options, method: "PATCH", revision });
    }
    revokeWorkerEnrollment(id, generation, reason, revision, options) {
        return this.mutate("workers/" + encodeURIComponent(id) + "/enrollment/revoke", { enrollment_generation: generation, reason }, { ...options, revision });
    }
    listRuntimeProfiles(signal) { return this.pages("runtime-profiles", 100, signal); }
    machineAction(id, action, generation, options) {
        return this.mutate(machine(id) + "/" + action, { expected_generation: generation }, options);
    }
    async machineMutation(id, suffix, body, options = {}) {
        const current = options.revision === undefined || options.generation === undefined
            ? await this.getMachine(id, options.signal) : undefined;
        return this.mutate(machine(id) + "/" + suffix, { ...body, expected_generation: options.generation ?? current?.binding_generation }, { ...options, revision: options.revision ?? current?.revision });
    }
    standby(id, activityPolicy, options) {
        return this.machineMutation(id, "standby", { activity_policy: activityPolicy }, options);
    }
    hibernate(id, activityPolicy, options) {
        return this.machineMutation(id, "hibernate", { activity_policy: activityPolicy }, options);
    }
    startTask(id, spec, options) {
        return this.machineMutation(id, "tasks", spec, options);
    }
    getTask(id, signal) { return this.get(task(id), signal); }
    async cancelTask(id, graceSeconds = 5, options = {}) {
        let revision = options.revision;
        if (revision === undefined) {
            const current = await this.getTask(id, options.signal);
            revision = (await this.getMachine(current.machine_id, options.signal)).revision;
        }
        return this.mutate(task(id) + "/cancel", { grace_seconds: graceSeconds }, { ...options, revision });
    }
    capture(id, purpose, label, { captureMode, ...options } = {}) {
        return this.machineMutation(id, "snapshots", { purpose, label, capture_mode: captureMode }, options);
    }
    checkpoint(id, label = "", options) {
        return this.machineMutation(id, "snapshots", { label, capture_mode: "vm" }, options);
    }
    clone(id, childName, options) {
        return this.machineMutation(id, "clones", { child_name: childName, copy_policy: "managed_clean" }, options);
    }
    async fork(id, childName, { requireIndependent, memoryForkBackend, ...options } = {}) {
        if (memoryForkBackend && !requireIndependent)
            throw new TypeError("memoryForkBackend requires requireIndependent: true");
        return this.machineMutation(id, "forks", { child_name: childName, require_independent: requireIndependent || undefined, memory_fork_backend: memoryForkBackend }, options);
    }
    forkBatch(id, request, options) {
        return this.machineMutation(id, "fork-batches", { require_independent: true, ...request }, options);
    }
    restore(snapshotID, name, { volumeRestorePolicy, ...options } = {}) {
        return this.mutate("restores", { snapshot_id: snapshotID, name, volume_restore_policy: volumeRestorePolicy }, options);
    }
    rollbackVM(id, snapshotID, volumeRestorePolicy = "clone_conflicts", options) {
        return this.machineMutation(id, "rollback", { snapshot_id: snapshotID, volume_restore_policy: volumeRestorePolicy }, options);
    }
    listTokens(signal) { return this.pages("tokens", 100, signal); }
    issueToken(spec, options) { return this.mutate("tokens", spec, options); }
    revokeToken(id, revision, options = {}) {
        return this.mutate("tokens/" + segment(id), {}, { ...options, method: "DELETE", revision });
    }
    async expose(id, spec, options = {}) {
        const revision = options.revision ?? (await this.getMachine(id, options.signal)).revision;
        return this.mutate(machine(id) + "/services", spec, { ...options, revision });
    }
}
