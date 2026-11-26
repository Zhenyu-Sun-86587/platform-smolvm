# A20 live Worker network recovery

Worker schema 28 records a receipt for each published TAP. The receipt ties the lease to the router and port network namespaces and their routes, link configuration and nftables policy. The Worker pins the router namespace in its private state directory. Consecutive Worker processes need to see that mount to recover the router.

Recovery uses the registered VMM's process handle and inventory to verify the live TAP. The operation must have completed, and its initial publication or target release must have committed. The Worker reopens DNS in the original namespace and takes ownership of the original attachment. An older lease without a receipt stays unknown after a restart. A changed policy or a missing attestation also keeps the lease fenced.

## Linux/KVM evidence

- The combined Native and FixedInternal fixture kept the original Guest TCP socket pair, process identities, TAP leases and writer OFDs while independently killing platformd, Serve and Worker with `SIGKILL`. Its seventh response matched the seventh unique request and the first 571 bytes of the attached volume. The independent scope-cut run also interrupted Serve before and after systemd scope assignment. Its original stream reached nine responses and 756 matching volume bytes. [Component restart fixture](../../tests/integration/component_restart_kvm.py)
- The supplemental run checked both Guest DNS labels across Worker replacement, including their assigned private addresses. Both stale revisions were rejected. A fresh dial to each original Guest executed exactly once, as recorded by the service in the Guest. Adding an ingress rule to the private router kept both TAPs unknown and blocked dials and `CompleteRecovery`. The Worker recovered when the original rules returned. The first public Stop preserved the other Guest's route. The final Stop retired both leases and unmounted the router namespace. [Network recovery fixture](../../tests/integration/worker_network_recovery_kvm.py)
- The Worker catalog test covered a published but still active launch, an unknown initial gate, and migration from schema 27 without a receipt. Each retained the unknown lease and refused to complete recovery. The Worker library suite passed 13 tests.

Both KVM runs used the installed `fb69434` VMM and reader. The development bundle replaced the Worker and its manifest. Its files were bound read-only for the supplemental run, and the original installation retained single-link files.

| Run | Bundle SHA-256 | Worker SHA-256 | Raw log |
| --- | --- | --- | --- |
| Component restart and scope cuts | `0c63db6dc6e68b0dd858aa3282716d330c1cf3b7af2dab14f087cc51b7f63d98` | `54ebecfb6dc92e9baaaba256408b68de245228dacb7a70afdf0529d43ebe15b8` | `.build/wp23/a20-stream-scope-network-v2b.log` |
| DNS, service dial and retirement | `6826fda568eaa22fde2f3c9be6528983429a23905da521a7de27a6dfd347d153` | `1e347929df430ead28c71e3be85540010a601e9405e0ba905fbd999baf1bed35` | `.build/wp23/a20-network-dns-dial-v3.log` |

Run the corresponding fixture on a Linux systemd host with KVM and a local Debian System image. Each fixture creates its own network and mount namespaces and a transient systemd slice.

```sh
sudo python3 -B tests/integration/component_restart_kvm.py \
  --bundle "$BUNDLE" --platformd "$PLATFORMD" --system-image "$SYSTEM_IMAGE" \
  --work-dir /dev/shm --scope-cuts
sudo python3 -B tests/integration/worker_network_recovery_kvm.py --authorized-only \
  --bundle "$BUNDLE" --platformd "$PLATFORMD" --system-image "$SYSTEM_IMAGE" \
  --work-dir /dev/shm
```

For a Worker host reboot, the fixture starts a fresh owner session on the second boot. It compares the retained control epoch and resumes the same saved cut. The temporary host observation stores the epoch, instance references and StatePoint. The two sessions come from separate logins.
