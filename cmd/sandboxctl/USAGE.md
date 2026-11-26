# Sandbox Platform CLI

Run `sandboxctl login --url https://PLATFORM --token-stdin` and send a
Platform API token on stdin. The CLI can also read `SANDBOX_TOKEN`. Use
`--ca-file` for a Platform CA missing from your system trust store. Use generated resource IDs in scripts
and arbitrated writes. Workspace commands also accept `name:DISPLAY NAME`
for an exact, unique display name. Quote a name containing spaces. For
mutation replay or explicit revision or generation guards, use the original
machine ID.

| Workflow | Commands and defaults |
| --- | --- |
| Workspace | `machines create NAME --template TEMPLATE` starts the new workspace by default. Use `--start=false` to leave it stopped. `machines show MACHINE`, `machines rename MACHINE NAME`, `machines config get MACHINE`, `machines config set MACHINE --body JSON`, and `machines resources MACHINE` expose requested and applied settings. Existing `machines patch` and `machines resize` remain available. |
| Guest | `exec MACHINE -- COMMAND ...` retains its bounded initial wait. `exec --follow MACHINE -- COMMAND ...` follows stdout and stderr through the Guest exit. `exec --tty MACHINE -- COMMAND ...` attaches a raw terminal. `ssh MACHINE [--identity FILE] [--batch-mode] [-- COMMAND ...]` opens OpenSSH using the platform tunnel and a verified Guest host key. It uses standard SSH remote shell semantics. The existing `ssh-config`, `ssh-host-key`, `execs output`, and `files list/get/put` remain available. `files list` accepts `--limit` and `--cursor`. `files get MACHINE PATH -` writes raw file bytes. |
| Live Fork | `machines live-fork MACHINE CHILD_NAME` requires native resource independence. `machines fork-batch MACHINE CHILD... [--admission all_or_reject\|best_effort] [--volume-policy clone]` requires native independent children and defaults to all-or-reject admission. `operations retry OPERATION` retries eligible failed targets from the original fork cut. |
| VM state | `snapshots capture-vm MACHINE [--label LABEL]`, `snapshots restore-vm SNAPSHOT NAME`, and `snapshots rollback-vm MACHINE SNAPSHOT` require a full VM state point. Restore and rollback accept `--volume-restore-policy exact\|clone_conflicts` and default to `clone_conflicts`. Use `snapshots get SNAPSHOT` for capture evidence and `operations get OPERATION` for the restore result and volume map. |
| Power | `machines standby` and `machines hibernate` preserve a qualified live execution. `machines wake-full MACHINE --from standby\|hibernated` restores the specified kind of preserved state. `machines cold-start MACHINE` requires a stopped source. Existing `start`, `stop`, `pause`, `resume`, and `wake` retain their prior dispatch rules. |
| Entry points | `services list MACHINE`, `services status SERVICE`, and `services endpoint SERVICE` report stored configuration and scoped observations. `port-forward SERVICE` opens an authorized tunnel. Passive service commands acquire no Guest access. |
| Storage and network | `volumes new NAME --capacity-bytes BYTES --runtime-instance RUNTIME` creates a block volume. `volumes snapshots` manages immutable entries and `volumes attachments` works with offline attachments. `networks` lists registered private networks, `networks attachment MACHINE` reads current attachment, and `networks set-defaults` edits the defaults for new System workspaces. |
| Automatic lifecycle | `lifecycle status MACHINE` shows desired and applied policy, blockers and any Worker evidence. `lifecycle set MACHINE --policy JSON` edits the override. `lifecycle defaults get` and `lifecycle defaults set --policy JSON` read and patch installation defaults. `leases acquire MACHINE --semantics-version 2`, `leases list`, `leases renew`, and `leases release` use the existing bounded running lease. The acquire default stays at version 1. |
| Legacy operations | `machines fork` retains its compatibility default and accepts the existing `--require-independent`. `snapshots capture` retains managed-task capture by default and accepts the existing `--capture-mode vm`. Generic restore and rollback follow the stored snapshot mode. |

A waited mutation writes its terminal operation to stdout. Exit status 0
means success, including an accepted mutation when using `--no-wait`.
Terminal failures, cancellation, recovery-required operations and API errors
use exit status 1. A succeeded operation retains exit status 0 when its effect
is partial or unknown. Pass `--strict-result` to require a confirmed complete result and
return 1 for partial or unknown effects. The operation's stage, effect and child results are
preserved in its output. A local operation wait timeout uses exit status
124 and reports the original operation ID. Resume with
`operations wait OPERATION`. When the server cannot confirm a mutation
effect, keep the original control epoch, idempotency key, revision, request
and target. The CLI prints the intent flags for a lost or uncertain reply. In JSON mode,
the single error object contains the original intent. The transport does not
resend a mutation once transmission may have begun. Verify the original
effect before deciding what to do next.

`--json` emits compact JSON for management results. API errors and wait
timeouts retain structured fields on stderr. Guest binary downloads,
PTY output and SSH remain raw streams. PTY input and window-size changes
are sent to the bound Guest execution. `exec --follow` copies stdout and
stderr independently and exits with the Guest status on a confirmed exit.
With `--json --follow`, exec emits one JSON result per observed output page.
An interrupted stream may have given the recipient a prefix of the output.

## Managed OpenSSH

`sandboxctl login --url https://PLATFORM --token-stdin` verifies and saves
the token in a private profile and installs a single Include in your
OpenSSH config. `--no-ssh-config` saves only the profile.
Use `--ssh-config PATH` to choose a separate OpenSSH configuration file
and point ssh, scp or VS Code Remote SSH at that file. The default profile
is named `default`. Select another using `--profile NAME` or `SANDBOX_PROFILE`.
The generated aliases use the selected saved origin and credential.

Use `ssh myapp.sandbox`, `scp file myapp.sandbox:/tmp/`, or select
`myapp.sandbox` in VS Code Remote SSH. A qualified System Guest and
an authorized private network are required. A machine-scoped manage token
can access its permitted workspace. The local
profile holds the Ed25519 user key and short-lived certificate. Connection
preparation runs through an exact alias, and OpenSSH verifies the platform's
current Guest host key. The generated configuration contains static IDs and
paths. The token remains in the private profile.

Aliases come from unique visible workspaces whose names are ASCII DNS
labels. With the OpenSSH Include installed, successful local machine
creation, fork, restore, rename, and deletion refresh aliases. After
`--no-wait`, use `sandboxctl operations wait OPERATION` or
`sandboxctl machines list` to refresh once changes are visible. Use
`sandboxctl ssh-config sync` to refresh aliases after changes from other
clients. An older mapping rejects a changed, deleted or ambiguous target.
The sync command prints the generated config path for use with
`ssh -F PATH` or an OpenSSH Include.

`sandboxctl ssh myapp` resolves the display name directly and uses an
ephemeral managed key. The direct command uses the published SSH service
for non-root users or when the Platform confirms managed certificates
are unsupported. Pass `--legacy` to select the published service with an
SSH agent, or `--identity FILE` to use a registered private key. An
explicit token uses the explicit URL or the usual localhost default.

The SSH shortcut uses OpenSSH with `KnownHostsCommand` support. The host-key
callback requires read access. Opening the managed tunnel requires manage
access to the machine. The proxy can wake the Guest under its existing
lifecycle policy.
