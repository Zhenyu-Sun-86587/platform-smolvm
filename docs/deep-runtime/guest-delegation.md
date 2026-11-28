# Guest delegation and host credential proxy

The installation owner creates a grant at `POST /v1/guest-grants` and issues its credential at `POST /v1/guest-grants/{grant}/credential`. Issuance returns the bearer once. Deliver the short-lived bearer to the designated Guest. Worker attestation binds each request to the Guest's current execution, including its incarnation and epoch. The public API and a forked Guest both reject that bearer.

An owner can grant `machine.create`, `machine.fork`, `machine.read`, `machine.exec`, or `credential_proxy.use`. Each grant defines an expiration, total uses, and a concurrent request limit. The default concurrency limit is four. Running relay calls and pending or uncertain delegated operations count toward the source Guest's concurrency. The authorization transaction checks the grant and reserves quota together with the operation. Retrying the same accepted intent reuses its operation ID and quota reservation.

For `machine.create`, set `limits.create` to the approved System template, runtime, vCPU count, memory in MiB, and optional native `network_id`. Set `limits.max_children` and `limits.max_total_memory_mib` to bound the Guest's delegated fleet. The relay accepts only the new machine's name. For `machine.fork`, set `target_machine_id` to the approved source and set the same two fleet limits. The relay accepts one independent child for that source and requires its current revision and binding generation. The installation owner owns the resulting machines, and the registry records the Guest principal that created them.

A Guest can read the control epoch at `GET /v1/control` and poll `GET /v1/operations/{id}` with a current credential for the grant that created the operation. A `machine.read` grant provides the revision and binding generation for its exact target. The Python SDK connects through Linux virtio-vsock to the host at port `6001` by default.

```python
from sandbox_platform.guest import GuestClient

guest = GuestClient(issued_guest_bearer)
intent = guest.intent()
operation = guest.create("analysis worker", intent=intent)
status = guest.operation(operation["operation_id"])
```

Revocation uses `DELETE /v1/guest-grants/{grant}` with the current grant revision. The dispatcher revalidates the recorded credential and grant before submitting a queued operation. If a submission already reached the Worker, normal receipt reconciliation accounts for its outcome. Active relay calls revalidate their authority and close on revocation.

## Proxy policy

Put the provider bearer in an owner-readable regular host file with mode `0600`. Register its fixed HTTPS origin, approved routes, and budgets in `platformd` configuration. The optional `ca_file` adds a trusted TLS root. Enable `allow_private` only for an explicitly registered private upstream.

```yaml
credential_proxies:
  - id: model
    upstream: https://models.example.com
    credential_file: /etc/sandbox-platform/model-bearer
    routes:
      - method: POST
        path: /v1/chat/completions
    requests_per_minute: 12
    request_bytes: 262144
    response_bytes: 4194304
    timeout_seconds: 60
```

Grant `credential_proxy.use` with `proxy_id: model`. The Guest sends its grant bearer to `/v1/credential-proxies/model/v1/chat/completions` over vsock. Platform verifies TLS, sets the host bearer on the outbound request, and rechecks the origin, route, and grant before following a redirect. The response streams in bounded chunks and redacts literal occurrences of the provider bearer. Audits record the proxy ID, method, status, and byte count. An in-flight model response holds the VM awake until the stream completes, disconnects, or is revoked.

```python
from sandbox_platform.guest import GuestClient

guest = GuestClient(issued_proxy_bearer)
with guest.proxy_stream("model", "/v1/chat/completions", method="POST",
                        body=provider_request_json, content_type="application/json",
                        accept="text/event-stream") as response:
    for line in response:
        consume(line)
```

Application secrets injected into a Guest environment can persist in its RAM and full-state copies. New task environment writes use the existing encrypted `task-env-v1` format and legacy references remain readable. The host proxy keeps its provider credential in an installation-managed host file.

## Verification

Run the Linux integration fixture with a KVM-capable host, a managed runtime bundle, and a Debian System ext4 image containing that bundle's measured identity helper and the installed task services. The `proxy` case creates an isolated TLS upstream and uses the real Worker activity collector. The `delegate` case boots a System source and exercises actual Guest-authorized create and independent Fork.

```sh
cc -static -Os -s -o /var/tmp/guest-probe tests/fixtures/vsock-probe.c
sudo python3 tests/integration/guest_control_kvm.py \
  --bundle "$bundle" --platformd "$platformd" --system-image "$system_image" \
  --probe /var/tmp/guest-probe --case delegate
sudo python3 tests/integration/guest_control_kvm.py \
  --bundle "$bundle" --platformd "$platformd" --system-image "$system_image" \
  --probe /var/tmp/guest-probe --case proxy
go test ./internal/lifecycle -run TestRevokedGuestCreateNeverReachesNativeLaunch
```

## Active model streams during Fork

The two public Fork entry points are `POST /v1/machines/{id}/forks` and `POST /v1/machines/{id}/fork-batches`. The source-stream regression uses an execution-bound grant and an active credential-proxy hold. The Guest submits one model POST to a local HTTPS provider, reads the first SSE event and waits. After the independent Fork completes, the provider releases the final event. The source must read that event exactly once on its original connection. Each child must see only the already-read prefix and wait for a separately authorized request. The provider checks the original request body and count. Grant revocation has a separate test that closes a live stream before the next event. These tests use a Debian Guest, the normal relay, and a local provider under test control.

The scope is an in-flight request through the managed credential proxy on a running source. A coding Agent's conversation history, tool ownership and child-session idle state require a separate session-level contract.

The integrated Worker passed the two-child HTTPS/SSE case with Native RAM on the stock kernel and with Kernel CoW RAM on the canary kernel. The source received the second event on its original connection, both children received only the first event on their inherited connection, and the provider recorded one original POST. The separate revocation case also passed. [Bundle and test records](kernel-cow-closeout-2026-11-28.md)
