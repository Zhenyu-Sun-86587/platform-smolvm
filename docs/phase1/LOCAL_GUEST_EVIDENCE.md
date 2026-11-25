# Managed guest and terminal validation

The gates ran on the local nested-KVM node on 2026-11-25 using official smolvm 1.37.1, Linux x86_64, virtio-net, internal storage, and the private socket at `/run/smolvm-phase1/api.sock`. G1 and Live Fork used the original managed archive with SHA-256 `51d03c40d3c6fd2bf6b0afd0a5bd1b03e404295f41aec0468d7aa3a2017bb17a`. The process census and subsequent capture gates used `debian-managed-v1-r2.tar`, SHA-256 `c2de1b9093d7c95efa4297dda5193b366f598bf76ea08f04c2bc05aed913984d`. The prepared Debian 12 tree included runit and OpenSSH. Build a Docker-format archive with [build-archive.sh](../../guest/templates/managed/build-archive.sh). The platform template's `spec.local_image` is the archive basename under the configured runtime image directory.

## G1 Guest supervision

- Native background exec started runit. Guest taskctl connected over the private Unix socket and confirmed cgroup v2 migration, freezing, and kill.
- A root task wrote inside `/etc`, produced byte-exact stdout and stderr, and returned exit code 7. Reading stderr from offset 2 with limit 2 returned the expected two bytes.
- Killing taskd with SIGKILL changed its PID from 15 to 104. Retrying the original start token found the same runner and one business start marker. An output reader resumed at its previous offset.
- Restarting the local official serve service retained Guest runner PID 137 and start time 5234. The output cursor advanced from 372 to 712.
- Cancelling the payload cgroup removed its subprocess and the cgroup. The runner supplied the durable cancelled receipt.
- OpenSSH public-key authentication and SFTP readback worked end to end as root using explicit known-host verification.

## G2 Active recovery

The guest rejected an unmanaged root process before entering the recovery barrier. After its removal the capture receipt identified the owned runner, frozen payload, and task manager. The standalone checkpoint was 163,748,583 bytes with SHA-256 `6812a275fd55ebbb3a1d2d35182a165ca2f0c7df0d993b7cad962b525469a880`.

The original runtime reached stopped through the public API. On the restored candidate's first execution the original cgroup still reported `frozen 1`, and the canary made no further writes. Default cancellation produced an `interrupted_by_restore` receipt with no exit code, a new task ID linked to its origin, and exactly one original launch marker. This run exercised the Guest protocol through the official runtime API.

## G3 Live Fork and standalone restore

- Native Branch succeeded while a root task, unmanaged background process, SSH connection, and native exec were active. The child's inherited SSH forward used a new host port.
- Guest adoption retained the captured runner, output, and process state with a new task ID and source task reference. Parent and child cursors advanced independently. Child cancellation and writes left the parent's running task and workspace intact. Child SSH identity rotated, and parent deletion returned the native dependency conflict while the child existed.
- Clean capture rejected an unregistered root process. Its authorized runit canary was explicitly registered with its executable in `/usr/local/share/sandbox-managed/capture-services.json`. The capture contained a 4 MiB RAM-only canary and a file canary. Source stopped and was deleted through public API before restore.
- A second official runtime used a separate Unix socket, rollout port, cache, and state directory. Its systemd mount namespace made the source state and image directories inaccessible. The 183,833,106-byte standalone checkpoint had SHA-256 `128a090fc2461f66d0ab525c757e9b2f1946fdff4b78820db33eb7ce97756f20`. The candidate started with its native ports cleared and outbound allow lists empty. It recovered the exact RAM and file canaries, rotated SSH identity, and accepted new writes.
- An immediate native Exec after Resume returned a transient conflict. A later fresh observation and Exec succeeded. The independent copy retained its writes through Pause/Resume and cold Stop/Start. A second standalone export and restore retained the writes. That artifact was 175,970,108 bytes with SHA-256 `3c6daf682ea1eb8020bdb1232d584adf6585fe3a7a155542c3abc23f54106fe9`.

## W04 Host API

A local platformd with independent SQLite state served an authenticated session against the managed archive. Short native root exec preserved separate stdout and stderr, exit code 7, and the same exec ID on idempotent replay. An authenticated terminal WebSocket negotiated the terminal subprotocol and carried ready sequence, resize to 113×30, stdin bytes, raw PTY output, and real exit code 5. Reusing the one-time ticket was rejected. A command emitted 750,000 bytes, and three concurrent output readers returned the same 512 KiB-window rollover gap and bounded data.

The files API listed `/etc`, downloaded the guest's `/etc/passwd`, and round-tripped a 4 MiB binary file exactly. Replaying the upload returned the same durable receipt.

## W06 signed artifacts and restore verification

The platform exported and re-imported a signed clean snapshot. Repeating the import with the original key returned the same operation and left the upload staging directory empty. The imported snapshot passed checksum verification and restored into a separate logical machine. Its binding pin refused snapshot deletion while the restored machine existed. Deleting that machine released the pin, and snapshot deletion then succeeded.

An online verification used `runtime.verify_socket` pointing at the isolated v1.37.1 runtime. The verifier checked the signed clean guest barrier after startup, kept port mappings and allow lists empty, stopped and deleted its temporary VM, and recorded `restore_verified_at` only after confirming absence. The operator provides a separate runtime state directory and prevents that runtime from reading the product state or image cache.

A managed rebuild with SHA-256 `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64` also restored a clean recovery-purpose snapshot on the isolated runtime and assigned a new logical guest task namespace.

## W07 rollback fencing

- The platform captured a signed active snapshot and rolled back running sources twice. Explicit `resume_captured` kept the same runner identity and resumed additional canary writes with one launch marker. Default cancellation preserved a frozen first execution, returned `interrupted_by_restore` without an exit code, and made no additional canary writes. Both original runtime names remained stopped.
- An old paused source without the explicit export-and-delete policy was rejected before candidate import, and its operation pin was released. With the policy, the platform exported and pinned the saved execution, used non-cascading deletion, confirmed the old name was absent, and advanced the binding. The protected artifact's task content remained unknown.
- A rollback from an already-stopped source preserved that stopped object and started a new execution. A disagreement in public native ownership labels preserved the old running source and operation pin, and the candidate name remained absent.
- The official status CLI helper reported `restart_policy=never` for the owned execution. The product fence compares its state and binding label with fresh runtime HTTP observation.
- On a separate runtime, a non-branchable native leaf entered a real memory-pressure pause. The public reason began with `host memory pressure:`. The public export produced 146,326,714 bytes. Native Stop returned a conflict, and a non-cascading Delete confirmed absence. The platform pressure-reason and inactive-fence cases have targeted Go coverage. The ordinary paused path had end-to-end API coverage. The pressure-specific platform rollback and a created source had narrower coverage.

## A08–A10 and G4 follow-up

The follow-up used a separate v1.37.1 serve with its own socket, cache, state root, and rollout port. The product API fixture used its own SQLite database and loopback listener.

- **A08** used the exact r3 archive. A disposable guest delayed its original runner after durable dispatch and before the runner identity record. Taskd received SIGKILL while the first request was in flight. After the restart the same token returned `unknown` with reason `dispatch_without_runner`. Releasing the original dispatch produced one launch, one task ID, and real exit code 23. A cold boot changed the boot ID, revalidated cgroup v2, and preserved the workspace canary and exit receipt.
- **A09** used `debian-managed-v1-r4.tar`, SHA-256 `e77444dc02446153136008fe4383132579381fdcd403ca73b476e4e646ece281`. A 13 MiB flood retained 10 MiB in two files and reported 3 MiB dropped. Four readers received identical bytes from a slice spanning the segment boundary. Both the guest-wide quota and the terminal retention clock reclaimed output while preserving exact cursor gaps and original exit receipts. The output limit also triggered the requested cancellation policy. A cold boot preserved the reclaimed boundary and exit code.
- **A10** three readers of an active native exec received the same bytes. Restarting the audit serve produced `session_lost` with no exit code. An idempotent replay returned the original exec ID, and the guest launch canary still contained one marker.
- **G4** the exact r3 archive reminted the inherited port 22 forward when Native Branch received `ports: []`. Native checkpoint import with `ports=[]` inherited the captured Guest port. A subsequent explicit PUT cleared it while the candidate was still created. First execution retained empty port and egress lists. Guest TCP probes to the audit management listener through its gateway and host address failed, and the audit control socket was absent inside the guest. The authenticated platform API round-tripped exact bytes through an `/etc` path and rejected a noncanonical Guest path.

The release builder's default is the exact r3 archive. Capability declarations are limited to digest and profile pairs in the release registry. An unlisted archive can be packaged with an explicit path and carries no verified capabilities. R4's evidence applies to persistent task supervision and log retention. [Managed archive evidence registry](../../packaging/managed-template-gates.json)

A release produced by the two-argument builder exposed the exact r3 digest, verified profile, and evidence location through `GET /v1/templates`. A machine created from that packaged template retained the same capability declarations and returned to Guest Management ready after a cold Stop/Start. Its workspace canary and task exit code 11 persisted. The installer plan verified all release checksums.

## Default r3 capability gates

The default image SHA-256 `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64` passed the following gates on a separate official smolvm 1.37.1 serve with virtio-net and full internal storage.

- **Live Fork** used `product.py --through fork`. Branch operation `op_3973e62eeac7d7a9d7c528f9231070db` adopted a running root Task into a new child task ID with source attribution. Child cancellation left the parent Task running. Both workspaces were deleted through the API.
- **Active recovery** used the public platform API and the Guest freeze gate. Snapshot `snap_214842d0d2fbb099c444108cda441508` was 146,596,582 bytes with SHA-256 `3fb3b603e4b9a018b63ae5e4bf38adeaf092ec860a84a9ab71cc4e350968f34f`. Rollback `op_8ce3809a03f1dc1b3b1e1cf67bf97545` resumed the original runner from a stopped source. Rollback `op_8edf78e0dd9535566377a3bbb71fc5f4` used default cancellation and published `interrupted_by_restore` with no exit code or additional canary writes. Both preserved one launch marker. The independent Guest check observed `frozen 1` on first restored execution before applying the cancellation policy.
- **Automatic idle** used `workspace.py --interrupt-dispatch`. A suspect lease blocked idle and metrics did not count as user activity. One durable wake followed the idle pause after the pending client disconnected. [Idle fixture record](../../tests/evidence/m3.md)

The verified profile is exposed through the same machine capability response used by the Web console and `sandboxctl machines get`.
