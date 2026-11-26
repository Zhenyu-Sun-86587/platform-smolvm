# Console reference and verification

The scene numbers match the JPEG filenames in the November 26 frontend reference package. A local Vite development server opens each scene at `/?fixture=NN`. The production bundle uses the authenticated platform SDK. A fixture query on the production server still requires login.

| Scenes | Surface and interaction | Production data or scope |
| --- | --- | --- |
| 01, 45 | Desktop and phone Dashboard, quick actions and machine cards | Machines, operations and available metrics. The developer introduction uses local previews. |
| 02, 04, 41, 46 | Machine list, stopped state, stacked phone cards and fine-pointer tablet | Machines, power, capabilities, resource configuration and navigation to the selected machine. The additional 46-coarse capture uses the touch drawer. |
| 03, 36, 43 | Centered create dialog, invalid name and mobile bottom sheet | Templates, runtimes, network profiles, platform name validation and create Operation. Extra fields follow the selected platform and boot mode. |
| 05, 16 | Snapshot directory and capture dialog | Snapshot collection, source machine, capture, restore and artifact actions. |
| 06, 17 | Disk directory and create volume dialog | Volumes, capacity, attachments, growth and volume snapshots. |
| 07, 26 | Global automations and the machine's automation tab | Existing persistent tasks and lifecycle policies. General cron scheduling is deferred. |
| 08, 28 | Agent directory and per-machine agent activity | Reference scenes and an explanation of available guest management and persistent tasks. AgentSession history is deferred. |
| 09 | Searchable integration cards | Development catalog with local brand icons. Third-party installation and OAuth are deferred. |
| 10, 22 | Domains directory and the machine networking tab | Domain binding preview, registered networks and real service configuration, observations, tickets and actions. |
| 11 | Global environment variable directory | Development preview. Task environments remain scoped to their tasks. |
| 12 | API keys, issue and revoke | Real token scopes, one-time secret display and confirmed revocation. |
| 13, 18 | Settings and team tab | Platform defaults, lifecycle settings, SSH keys and client certificates. Organization, team and billing scenes use development data. |
| 14, 20, 35 | Command menu, filtered result and keyboard highlight | Real routes and machine collection, recent visits, pointer and keyboard selection. |
| 15, 19 | Region and power-state filters | Reference regions in development. Production filters use available runtime and machine data. |
| 21, 37, 44 | Stopped, running and mobile machine Overview | Configuration, health, observed usage and execution identity. The three charts describe CPU time, runtime memory and disk observations in their reported units. |
| 23, 24 | Machine checkpoints and backups | Captured configuration, historical unknowns, restore actions and platform backup policies/runs. |
| 25 | Forks, family graph and fork actions | Independent forks, live forks, batches and real operation results according to capability and power. |
| 27, 38 | Files with a stopped guest and a running guest | Paged native listings, bounded text preview/edit, binary transfers and unknown write verification. Unreported permissions and timestamps have placeholders. |
| 29 | Machine settings and power | Resource configuration, lifecycle overrides, blockers, leases and current Worker state. |
| 30 | Open dialog | CLI, web terminal, desktop profile and existing sessions. Editor links and phone pairing use a labeled development preview. |
| 31 | Full-page terminal | xterm, ticketed PTY attach, input sequence, output cursor, resize and reconnect. |
| 32, 33 | Desktop connecting and display | noVNC and a private VNC service with a session bound to its machine execution. |
| 34, 39, 40 | Launcher loading, available apps and empty search | Development launcher. Production Desktop displays the guest framebuffer. |
| 42 | Mobile navigation | Touch drawer, route selection, search, account and keyboard focus. |

## Reproduce

Use the frontend worktree and the coordinating agent’s published contract and runtime paths. Build the web app with `npm ci` and `npm run build` in `web`, then build `./cmd/platformd` to embed it. Serialize these operations using the shared heavy lock. The integration tree must regenerate the source offer because it includes backend and SDK source.

- `tests/integration/console-reference.mjs` captures all 46 scenes at the original JPEG dimensions and an extra coarse-pointer tablet. It records route, headings, bounds, fonts, request isolation, overflow and keyboard assertions in `observations.json`. The local run also produced reference/implementation contact sheets.
- `tests/integration/console-credentials.mjs` starts an isolated production server, verifies failed authentication, validates token scope, issues a key and checks its one-time display and revocation.
- `tests/integration/console_machine_kvm.py` uses the existing managed System fixture and launches `console-machine.mjs`. The suites cover actual creation, file and PTY I/O, current metrics, a VM capture and a private VNC session. For the full suite give it a disposable Debian test disk containing Xvfb and x11vnc. Use the published `test-system` wrapper to acquire the heavy lock and then the worker-lab lock. The default browser path exercises the built-in web server.

Pass the installed Chromium and Playwright paths via the command’s flags or the browser scripts’ `CHROMIUM` and `PLAYWRIGHT_CORE` environment variables. The formal handoff records the exact builds, runtime digest, local evidence directory, results and observed differences.
