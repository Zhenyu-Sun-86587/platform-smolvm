# Scheduled backups

Configure a local backup destination and a separate 32-byte recovery key file readable only by the platform service. For an external destination, grant the systemd unit write access to its directory. Keep an independent copy of the recovery key.

```yaml
backups:
  - id: hourly
    scope: durable_runtime
    directory: /var/lib/sandbox-platform/backups
    interval_seconds: 3600
    retain: 3
    recovery_key_file: /etc/sandbox-platform/backup-recovery.key
```

The `platform_snapshots` scope captures the consistent registry and ready managed-task snapshot artifacts. The `durable_runtime` scope adds ready VM StatePoints, active Hibernate images, immutable Volume snapshots, their complete disk ancestry and the corresponding RuntimeBundles. To include a running machine or the latest data from a Volume, create a VM checkpoint or Volume snapshot before the run.

Use `sandboxctl backups policies`, `sandboxctl backups list` and `sandboxctl backups run hourly` to inspect or request a run. The console provides the same controls under Settings → Backups. New archives are verified before publication. A failed run preserves earlier verified archives. Retention operates on the previous verified runs belonging to the same policy.

```sh
sandboxctl admin backup verify --archive "$archive" --recovery-key-file "$key"
sandboxctl admin backup recover-native --archive "$archive" --recovery-key-file "$key" --output "$new_directory"
```

Verification authenticates the manifest, reads every listed file and checks the original StatePoint and block digests with the pinned reader. Recovery material for the installation signer, managed SSH identities and task environments is stored in an encrypted file. `recover-native` creates an isolated directory of verified native StatePoint and disk representations. Use a matching installed Worker reader with the bundle digest and reader ID declared by the backup.

Add `--worker-catalog` to prepare a new Worker catalog under each recovered route directory. Start a fresh Worker and a separate Serve instance against that catalog, then restore the selected StatePoint in clone-conflicts mode into a new logical machine.

For a networked StatePoint, register a target private network on the new Worker using a distinct route domain and a disjoint private CIDR. Use the target network ID as `network_id` in the private Serve `vm_restore` operation parameters. The Worker validates the archived attachment, keeps the original network mode and allocates a fresh routed address and attachment. It refuses missing, unknown or overlapping networks with `E_NETWORK_RECOVERY_UNAVAILABLE`. Restore only captures produced with the original network attestation present. Worker-private access and packet wake grants must be configured on the new Worker.

Successful network recovery reports the source and target attachment IDs, route domains, private addresses and in-Guest addresses in `statepoint.network_recovery`. In Native mode the Guest receives its new private address. Fixed internal mode retains the Guest address while the new Worker allocates a private address in its own route domain.

The SSE record includes `event_name`, `resource_kind`, `resource_id` and `schema_version`. Use the `Last-Event-ID` cursor to resume and deduplicate records. A `410` response requires reading current resources and starting a new stream. Volume events follow the owner scope. Auth and backup events require administrator scope.
