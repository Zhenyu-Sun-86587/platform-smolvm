# Sandbox Platform CLI

Set `SANDBOX_URL` and `SANDBOX_TOKEN`. Use `--ca-file` for a Platform CA that
is absent from your system trust store. Use generated resource IDs in scripts
and arbitrated writes. Workspace commands also accept `name:DISPLAY NAME`
for an exact, unique display name. Quote a name containing spaces. For
mutation replay or explicit revision or generation guards, use the original
machine ID.

| Workflow | Commands and defaults |
| --- | --- |
| Workspace | `machines create NAME --template TEMPLATE` starts the new workspace by default. Use `--start=false` to leave it stopped. `machines show MACHINE`, `machines rename MACHINE NAME`, `machines config get MACHINE`, `machines config set MACHINE --body JSON`, and `machines resources MACHINE` expose requested and applied settings. Existing `machines patch` and `machines resize` remain available. |
| Guest | `exec MACHINE -- COMMAND ...` retains its bounded initial wait. `exec --follow MACHINE -- COMMAND ...` follows stdout and stderr through the Guest exit. `exec --tty MACHINE -- COMMAND ...` attaches a raw terminal. `ssh MACHINE` opens OpenSSH using the platform tunnel and a verified Guest host key. The existing `ssh-config`, `ssh-host-key`, `execs output`, and `files list/get/put` remain available. `files get MACHINE PATH -` writes raw file bytes. |
| Live Fork | `machines live-fork MACHINE CHILD_NAME` requires native resource independence. `machines fork-batch MACHINE CHILD... [--admission all_or_reject\|best_effort] [--volume-policy clone]` requires native independent children and defaults to all-or-reject admission. `operations retry OPERATION` retries eligible failed targets from the original fork cut. |
| VM state | `snapshots capture-vm MACHINE [--label LABEL]`, `snapshots restore-vm SNAPSHOT NAME`, and `snapshots rollback-vm MACHINE SNAPSHOT` require a full VM state point. Restore and rollback accept `--volume-restore-policy exact\|clone_conflicts` and default to `clone_conflicts`. Use `snapshots get SNAPSHOT` for capture evidence and `operations get OPERATION` for the restore result and volume map. |
| Power | `machines standby` and `machines hibernate` preserve a qualified live execution. `machines wake-full MACHINE --from standby\|hibernated` restores the specified kind of preserved state. `machines cold-start MACHINE` requires a stopped source. Existing `start`, `stop`, `pause`, `resume`, and `wake` retain their prior dispatch rules. |
| Entry points | `services list MACHINE`, `services status SERVICE`, and `services endpoint SERVICE` report stored configuration and scoped observations. `port-forward SERVICE` opens an authorized tunnel. Passive service commands acquire no Guest access. |
| Storage and network | `volumes` manages block storage, immutable snapshots and offline attachments. `networks` lists registered private networks. `machines network MACHINE` reads the current attachment. |
| Automatic lifecycle | `lifecycle status MACHINE` shows desired and applied policy, blockers and any Worker evidence. `lifecycle set MACHINE --policy JSON` edits the override. `lifecycle defaults get` and `lifecycle defaults set --policy JSON` read and patch installation defaults. `leases acquire MACHINE --semantics-version 2`, `leases list`, `leases renew`, and `leases release` use the existing bounded running lease. The acquire default stays at version 1. |
| Legacy operations | `machines fork` retains its compatibility default and accepts the existing `--require-independent`. `snapshots capture` retains managed-task capture by default and accepts the existing `--capture-mode vm`. Generic restore and rollback follow the stored snapshot mode. |

A waited mutation writes its terminal operation to stdout. Exit status 0
means success, including an accepted mutation when using `--no-wait`.
Terminal failures, cancellation, recovery-required operations, partial or
unknown effects and API errors use exit status 1. The operation's stage, effect and child results are
preserved in its output. A local operation wait timeout uses exit status
124 and reports the original operation ID. Resume with
`operations wait OPERATION`. When the server cannot confirm a mutation
effect, keep the original control epoch, idempotency key, revision, request
and target. The CLI prints the intent flags for a lost or uncertain reply.

`--json` emits compact JSON for management results. API errors and wait
timeouts retain structured fields on stderr. Guest binary downloads,
PTY output and SSH remain raw streams. `exec --follow` copies stdout and
stderr independently and exits with the Guest status on a confirmed exit.
With `--json --follow`, exec emits one JSON result per observed output page.
An interrupted stream may have given the recipient a prefix of the output.
