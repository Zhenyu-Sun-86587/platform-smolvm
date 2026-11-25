# Local W10 client acceptance

Executed on 2026-11-25 with official smolvm 1.37.1, local KVM, a pinned Debian managed Guest image, and an independent runtime for restore verification. The client test platform used its own database, artifacts and credentials, a test certificate, the loopback aliases `console.localhost` and `*.apps.localhost`, and the release-built embedded console. All requests went to the local machine. [Agent workflows](../../examples/README.md)

| Client | Observed result |
| --- | --- |
| Browser, Chromium 133 | Signed in with the owner session and created a running workspace. Opened xterm using the one-time attach ticket, sent input, read the resized PTY dimensions, closed the WebSocket and reattached to the original session at its cursor. Started a persistent task and read both output markers through completion. Reloaded the console and found the original operation history. Opened a stable HTTPS service in a new tab through the one-time preview POST with HTTP/2 enabled. Uploaded a Guest file and opened its original bytes in the text editor. Issued a read token, observed it in the credential list, revoked it and received HTTP 401 on the next API call using that token. |
| Cobra CLI, Go 1.22.2 | Created and inspected a 512 MiB workspace, observed the original operation and ran a native command. Read the first persistent task log page, resumed from its returned byte offset and confirmed exact output and exit 0. Captured a clean checkpoint, verified its checksum, restored an independent workspace and served the demo response over stable HTTPS. Read the same response through local WebSocket port forwarding. Issued and revoked a scoped token. Used the generated OpenSSH configuration and the platform-verified Guest host key to log in and transfer a file over SFTP with a byte-for-byte match. |
| Python 3.12.3, httpx 0.28.1 | Ran `SANDBOX_EXAMPLE_SERVICE=1 python3 examples/agent_workflow.py`. Created the workspace, accepted the persistent task, closed the client after reading its first output chunk and resumed at its saved byte offset. Received all three output lines and exit 0. Captured a clean snapshot, completed Independent Clone and got HTTP 200 with the expected body through the clone's stable service. Cancelled the demo task. |
| TypeScript, Node 22.23.4 | Ran `SANDBOX_EXAMPLE_SERVICE=1 node examples/agent-workflow.mjs` against the built client. Created the workspace, accepted the persistent task, replaced the client after reading the first output chunk and resumed using its byte cursor. Received all three output lines and exit 0. Captured a clean snapshot, completed Independent Clone and verified the expected body from its HTTPS service before cancelling the demo task. |

The console submitted restore verification for the same clean snapshot and showed the completed operation `op_5795ba0c8402999145eb3435c63e5249`. The console’s restores filter displayed the completed restore history.

The CLI also completed `snapshots verify --verification=restore` using operation `op_5e454a2cabde0c1f6e9aaf10ce47b46c`. The terminal result identified verification runtime object `verify_5e454a2cabde0c1f6e9aaf10ce47b46c`. The restore history endpoint returned only restore operations, including `op_6b04e616eac11d426a55b91ebea8ddec`.

The first Chromium preview received HTTP 403 even though the cross-origin POST sent the console's correct `Origin`. Chromium had coalesced management and service HTTP/2 requests onto one TLS connection. The Gateway now returns HTTP 421 for an SNI and authority mismatch so Chromium retries on the correct connection. The final run exercised the normal browser security and HTTP/2 settings. The first SDK rehearsal also exposed a missing test-specific status socket configuration and a Guest image without Python. The completed runs used the phase-one status helper and the Perl demo included in the repository.

`go test ./...`, the pinned Vite and TypeScript SDK builds, the Python and Node example syntax checks, and OpenAPI YAML and local reference checks passed. The browser pass used Chromium on Linux.

The completed test database's 12 workspaces were deleted through the platform API after releasing native sessions and cancelling any active demo tasks.

## Service and credential write acceptance

`tests/integration/mutation_ingress.py` passed against the local TLS Gateway and smolvm 1.37.1. The fixture used two real VMs, a real HTTP readiness response and a scoped certificate signed by the test client CA.

- A stale service PATCH returned `REVISION_CONFLICT` with no corresponding accepted operation. Two concurrent PATCH requests with the same valid revision produced one accepted operation. The readiness policy blocked HTTP forwarding until the expected status matched the Guest's actual 200.
- Service DELETE rejected a stale revision at acceptance. After the service row was gone, the original key and revision returned the original operation. A new key returned 404.
- Credential issue and list reported revision 1. API token issue replay returned metadata without the secret. Token and certificate revoke required `If-Match`, returned revision 2, replayed with the original key and revision, and rejected a new key using revision 1.
- The machine-scoped token failed with HTTP 403 on the other machine's tunnel. The service-scoped certificate completed a TLS 1.2 handshake on the other service and could not send application data through it. Both checks left the target paused and its operation history unchanged. The same certificate reached its authorized service.

The Cobra CLI resolved the listed credential revision before revoking. The Python and TypeScript SDK token methods each issued and replayed, then revoked and replayed with the original revision. Both mutation-test workspaces were deleted through the API.

Chromium also read the issued token's revision and sent `If-Match: "1"` on revoke. Replaying an issuance receipt persisted by the earlier server returned its original token ID with revision 1 and the secret remained omitted.

## Reconcile after an unavailable status helper

The first Independent Clone rehearsal retained `op_aba6aba41ab7583a7531b0c96a5a569d` in `recovery_required` at `verify` after the test server tried a status socket outside the isolated setup. After correcting the socket and restarting platformd with the reconcile fix, the original operation completed. The candidate used the original native name `sbox_cef4742d98a1fd2ef552a0441d3afcc6` and generation 1. The original operation pin on `snap_6064caaf6e21de590f2fe25cb683a673` was present before reconcile and released on success. The recovered child and source were then deleted through the platform API with operations `op_944aa1e3318a15bd0b3974a332f11e00` and `op_462c6df257bcac4bf734dccea470140c`.
