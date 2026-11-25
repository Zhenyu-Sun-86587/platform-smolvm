# Sandbox Platform release and maintenance

Build on Linux x86_64 with Go, a C compiler, jq, and a prepared managed Docker-save archive. The Guest template builder is in [guest/templates/managed](../../guest/templates/managed).

```sh
packaging/install/make-release.sh 0.1.0 /path/to/debian-managed.tar ./release-0.1.0
./release-0.1.0/install.sh --plan --manifest ./release-0.1.0/release-manifest.json \
  --config ./release-0.1.0/config.example.yaml
```

The release manifest records the platform binaries, migration version, template, units and helpers. The runtime manifest pins the official smolvm 1.37.1 Linux x86_64 archive and bundled components. Install from an authenticated copy of the platform release. The SHA-256 checks verify integrity against those pins. [Official smolvm release](https://github.com/smol-machines/smolvm/releases/tag/v1.37.1)

The default archive for a release is `debian-managed-v1-r3.tar`, SHA-256 `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64`. The two-argument build looks for the verified archive in `/var/lib/smolvm-phase1-images` or in `SANDBOX_MANAGED_TEMPLATE_DIR` if set. Installed assets use the digest-derived image filename. The installed template catalog exposes the same digest, profile, and evidence. The verified r3 definition is catalog version 2 with ID `debian-managed-ad5b45167c99-v2`. Upgrades retain prior immutable template definitions and the machines that reference them.

```sh
SANDBOX_MANAGED_TEMPLATE_DIR=/path/to/verified-artifacts \
  packaging/install/make-release.sh 0.1.0 ./release-0.1.0
```

The explicit three-argument build also accepts other archives. The builder assigns verified capabilities only to exact digests recorded for the pinned runtime and architecture. A newly built archive with a different SHA-256 starts with an empty capability declaration. [Managed archive evidence registry](../managed-template-gates.json) [Local gate records](../../docs/phase1/LOCAL_GUEST_EVIDENCE.md)

Build a self-contained release archive and checksum from a committed source tree. The archive includes the pinned official runtime for offline installation and identifies the source commit in BUILD-INFO.json.

```sh
packaging/install/archive-release.sh 0.1.0 /path/to/smolvm-1.37.1-linux-x86_64.tar.gz .build/releases
cd .build/releases
sha256sum -c sandbox-platform-0.1.0-linux-amd64.tar.gz.sha256
tar -xzf sandbox-platform-0.1.0-linux-amd64.tar.gz
```

The optional fourth argument selects a Git reference. The default is the current commit.

## Install

Prepare an owner password file with mode 0600 and an operator configuration based on the included example. The example binds management to loopback on port 8443. Configure TLS and a distinct management origin before changing that bind to a public address. The units expect the fixed runtime and platform directories in the example.

```sh
sudo ./release-0.1.0/install.sh \
  --manifest ./release-0.1.0/release-manifest.json \
  --config ./release-0.1.0/config.example.yaml \
  --owner-password-file /secure/path/owner-password
sudo sandboxctl doctor --json
```

Use `--runtime-archive /path/to/smolvm-1.37.1-linux-x86_64.tar.gz` for an offline install. The same digest check applies. Systemd runs platformd as `sandbox-platform` and smolvm serve as root with native VM scopes and its per-VM UID isolation. The full runtime API stays on `/run/smolvm-platform/api.sock`. The separate local status socket accepts a single platform-generated machine name and runs the fixed public read-only smolvm status query as the runtime owner. The platform service account is the only named peer allowed by that helper.

### Service domains and independent restore verification

For a deployment using HTTPS, configure the management origin and the application domain separately. Point the management DNS record and the wildcard service record at the gateway. The TLS certificate must cover both the management hostname and `*.apps.example.test` in this example. The external listener can use port 443 directly or forward it to the platform listener while preserving the original TLS authority.

```yaml
api:
  listen: 127.0.0.1:8443
  public_origin: https://console.example.test
  tls_cert_file: /etc/sandbox-platform/tls/server.pem
  tls_key_file: /etc/sandbox-platform/tls/server.key
gateway:
  apps_domain: apps.example.test
  client_ca_file: /etc/sandbox-platform/tls/client-ca.pem
  client_ca_key_file: /etc/sandbox-platform/tls/client-ca.key
  tcp_mtls_port_range: [21000, 21063]
```

The client CA signs service-scoped CSRs for TCP mTLS. Keep the private files readable by the installed platform service account and restrict other users. HTTP/SSH tunnels use the management and service listener. TCP mTLS binds its allocated port on the address configured for the API listener. DNS, TLS material and external port forwarding belong to the deployment.

Checksum verification uses the local signed catalog. For an online test restore, configure `runtime.verify_socket` with the private socket of a separate smolvm 1.37.1 serve instance. Give the verifier its own data directory, socket and guest-rollout port, and grant the platform service account access to that socket. The verifier imports into a temporary named candidate, checks the recovered guest and disposes of the candidate. A single-runtime installation reports the missing verification prerequisite.


## Backup and recovery

The output directory must be new. Path selection is local to the administrator. A backup contains a consistent SQLite copy, the operator configuration, and the signed standalone artifacts referenced by ready catalog entries. Preserve the owner bootstrap credential, trusted signer configuration and any private recovery keys separately in encrypted operator storage.

```sh
sudo sandboxctl admin backup create --output /mnt/sandbox-backup/release-backup
sudo sandboxctl admin backup verify --archive /mnt/sandbox-backup/release-backup
sudo sandboxctl diagnostics export --output /var/tmp/sandbox-diagnostics.zip
```

Restore uses the installation marker and takes the same writer lock as platformd. Keep the installation identity, config and trusted signer material with your recovery procedure. The restore preserves a copy of the current DB and artifacts, rotates the control epoch, and holds older unfinished operations for recovery with unknown effects. Run the maintenance gate with the daemon stopped.

```sh
sudo systemctl stop platformd.service
sudo sandboxctl admin maintenance enter --reason database-recovery
sudo sandboxctl admin backup restore --archive /mnt/sandbox-backup/release-backup \
  --rollback-output /mnt/sandbox-backup/before-recovery
sudo systemctl start platformd.service
sudo sandboxctl doctor --json
```

Review the observed runtime machines, task state and unfinished operations while the gate is active. Then stop platformd, run `sudo sandboxctl admin maintenance exit`, and start it again.

## Upgrade and uninstall

Keep the verified release bundle available. The installed updater accepts platform releases for the pinned runtime. It refuses a lower database schema. Binary rollback requires matching schema and runtime versions. For a failed schema-changing upgrade, keep the maintenance gate in place and use the pre-upgrade backup with its compatible binaries.

```sh
sudo sandboxctl admin upgrade plan --manifest ./release-0.1.1/release-manifest.json
sudo systemctl stop platformd.service
sudo sandboxctl admin maintenance enter --reason release-upgrade
sudo sandboxctl admin backup create --output /mnt/sandbox-backup/before-upgrade
sudo sandboxctl admin upgrade apply --manifest ./release-0.1.1/release-manifest.json \
  --backup /mnt/sandbox-backup/before-upgrade
sudo sandboxctl doctor --json
```

Use `sudo sandboxctl admin upgrade rollback --to 0.1.0 --backup /mnt/sandbox-backup/before-upgrade` for a compatible installed release. After review, stop platformd, exit maintenance, and start the service.

```sh
sudo /opt/sandbox-platform/libexec/install-release --uninstall
```

Uninstall disables and removes the platform service and installed CLI link. It retains versioned release assets, platform data and configuration, runtime service and data, and backup files. An explicitly selected data-preserving reinstall uses `--reuse-data` and an unused release version.
