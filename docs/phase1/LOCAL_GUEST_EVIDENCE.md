# Managed guest and terminal validation

The gates ran on the local nested-KVM node on 2026-11-25 using official smolvm 1.37.1, Linux x86_64, virtio-net, internal storage, and the private socket at `/run/smolvm-phase1/api.sock`. The tested managed archive was `debian-managed-v1-r2.tar`, SHA-256 `c2de1b9093d7c95efa4297dda5193b366f598bf76ea08f04c2bc05aed913984d`. The prepared Debian 12 tree included runit and OpenSSH. Build a Docker-format archive with [build-archive.sh](../../guest/templates/managed/build-archive.sh). The platform template's `spec.local_image` is the archive basename under the configured runtime image directory.

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
