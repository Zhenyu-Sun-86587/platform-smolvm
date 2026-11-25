# Local release acceptance for 0.1.6

The Linux x86_64 offline release was built from `10bca47b1c0cc35a9db69066cbfd5ffa762d692e` on pve-bot. The archive contains the official smolvm 1.37.1 runtime, the managed r3 image, the platform binaries, and the embedded console. It records the exact source commit in BUILD-INFO.json. [Installer and maintenance guide](../../packaging/install/README.md)

| Workspace path | Bytes |
|---|---:|
| `.build/releases/sandbox-platform-0.1.6-linux-amd64.tar.gz` | 124187774 |
| `.build/releases/sandbox-platform-0.1.6-linux-amd64.tar.gz.sha256` | 108 |

Archive SHA-256 is `7949b70b2cedf3d6899107e653c8b17a80a96e0a22efe9112519b7f4752a4c3f`.

```sh
cd .build/releases
sha256sum -c sandbox-platform-0.1.6-linux-amd64.tar.gz.sha256
tar -xzf sandbox-platform-0.1.6-linux-amd64.tar.gz
```

The extracted installer verified the release manifest and embedded runtime archive and upgraded the dedicated installation from 0.1.4 after a verified local backup. The installed platformd, sandboxctl, template catalog and updater matched the package bytes. smolvm serve kept its PID during the update. The database stayed at schema 2 and retained control epoch `2814c0b9-b453-42f8-be15-14d333fb356c`. Maintenance was exited after verification.

The installed CLI at `/usr/local/bin/sandboxctl` reports version 0.1.6. `platformd.service`, `smolvm-serve.service`, and `smolvm-status.socket` are active and enabled. The management listener binds to `127.0.0.1:8443`. The console is available from that host at `http://127.0.0.1:8443`, with login account `owner` and the existing password file `/etc/sandbox-platform/owner-password` at mode 0600. Local diagnostics are available with `sudo /usr/local/bin/sandboxctl doctor --json`. Set `SANDBOX_URL=http://127.0.0.1:8443` for authenticated CLI API calls to this default configuration.

Chromium signed in to the installed HTTP console and received HTTP 200 from the login, control, templates, machines and logout routes. The control response reported build 0.1.6, runtime 1.37.1 and readiness. The catalog showed existing r2 alongside the installed default r3 definition `debian-managed-ad5b45167c99-v2`.

A separate upgrade test started the earlier daemon with the r3 v1 catalog in a temporary SQLite registry and persisted a machine row referencing that definition. The updated daemon opened the same database, preserved the old definition and machine reference, and registered v2 with the same image digest and image filename. The upgraded definition adds the verified Live Fork, active recovery and automatic idle capability metadata. [Managed r3 gate evidence](../../docs/phase1/LOCAL_GUEST_EVIDENCE.md)

The installed product smoke created and started workspace `m_4f0d04d7d1ec274bc56748806599a126` from r3 v2. Its managed task preserved both stdout chunks across cursor reads and reported the expected exit code 7. Create, task start, and cleanup completed as `op_3e51acfc35dfc994eb075f75647e7435`, `op_6c7eeec40b45170ab5c88e0ba99de2cb`, and `op_d6e4f66288b84654dab7ed021c0a0ef1`. The disposable workspace was deleted and the services remained ready.
