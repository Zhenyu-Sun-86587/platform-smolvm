# Deep runtime bundle tooling

`bundle.py` runs the platform-side build and packaging checks for Linux x86-64 with KVM and `BLK=1 NET=1 GPU=0`. Use Python 3.8 or later, Git, GNU binutils and GNU coreutils. The compiler is responsible for producing and recording fresh outputs in a separate builder workspace. Run the preflight before compilation and keep the locked source trees clean.

```sh
python3 packaging/deep/bundle.py check-inputs \
  --smolvm "$SMOLVM_CHECKOUT" --kernel-archive "$KERNEL_ARCHIVE" \
  --build-lock "$BUILD_LOCK" --toolchain-lock "$TOOLCHAIN_LOCK" \
  --builder-image "$RESOLVED_BUILDER_IMAGE"
python3 packaging/deep/bundle.py check-libraries --artifacts "$LIBRARY_OUTPUTS"
python3 packaging/deep/bundle.py check-runtime --artifacts "$RUNTIME_OUTPUTS"
python3 packaging/deep/bundle.py pack --artifacts "$ARTIFACTS" --output "$BUNDLE"
python3 packaging/deep/bundle.py verify "$BUNDLE" --expect "$EXPECTED_BUNDLE_DIGEST"
python3 packaging/deep/bundle.py install "$BUNDLE" \
  --expect "$EXPECTED_BUNDLE_DIGEST" --output "$INSTALLED_BUNDLE"
```

`check-inputs` requires the exact three Git commits and trees, matching committed gitlinks, and clean worktrees including ignored paths. It rejects Git LFS pointers in checked-out regular files. It compares the kernel archive, patch registry files, toolchain lock and dependency lockfiles against their pinned SHA-256 digests. Pass the builder image reference resolved by the runner. The command also checks the tool versions visible from the smolvm checkout.

## Builder metadata

Populate these files with values resolved by the builder. SHA-256 fields in the metadata use the `sha256:` prefix and source commit/tree IDs are 40 lowercase hex characters.

| Artifact | Schema and required fields |
| --- | --- |
| `metadata/build-lock.json` | `RuntimeBuildLock/1`. `sources` maps `smolvm`, `libkrun` and `libkrunfw` to `{commit, tree}`. `gitlinks` maps `libkrun` and `libkrunfw` to their relative checkout paths. `kernel` contains `release`, `archive_sha256` and `required_config`, a map from Kconfig symbols to their exact build values. Use `"n"` for disabled entries. Include `toolchain_lock_digest`, `flags`, ordered `patches` entries of `{id, repository, path, sha256}`, `required_libkrun_exports` and the `maximum_glibc` version. |
| `metadata/toolchain-lock.json` | `RuntimeToolchainLock/1`. Include `builder_image` as a registry image reference with a SHA-256 digest, `versions` with the installed `rust`, `go`, `node`, `python`, `cc` and `ld` versions, and `lockfiles` entries of `{repository, path, sha256}` covering at least the smolvm and libkrun Cargo locks. |
| `metadata/build-record.json` | `RuntimeBuildRecord/1`. Include `build_lock_digest`, computed over the lock file bytes, and `outputs`, a map from staged relative paths to the builder's output SHA-256 digests. Preserve this file from the build that produced the artifacts. |
| `metadata/rootfs-input-lock.json` | `RuntimeRootfsSeed/1`. Copy the committed seed lock byte for byte. Include `recipe` set to `scripts/build-agent-rootfs.sh` and the `seed_sha256`, `crun_build_sha256`, `package_db_sha256` and `crane_sha256` digests. Include the original smolvm-relative lock path and digest in the toolchain lock's `lockfiles`. |
| `metadata/rootfs-provenance.json` | `RuntimeRootfsProvenance/1`. New builds use `RuntimeRootfsProvenance/2` with the additional `identity_helper_sha256` field and a toolchain lock entry for `crates/smolvm-identity-helper/Cargo.toml`. The verifier checks the staged static helper against `/sbin/smolvm-identity-helper` in the OCI archive. Earlier stages use `RuntimeRootfsProvenance/1`. Preserve the assembler's record of the four pinned seed digests, `input_lock_path`, `input_lock_sha256`, `recipe_sha256`, `agent_sha256` and `rootfs_sha256`. If the toolchain lock pins the recipe, the provenance must match its digest. |
| `profiles/verification.json` | `RuntimeVerificationProfile/1`. Include `boot_profiles` with `oci-v1` and optionally `system-debian-v1`, exact `state_reader_id`, and `contracts` with version 1 for `worker_resource`, `capture_control`, `statepoint`, `guest_identity`, `network_attachment` and `block_attachment`. Include the nonempty `required_kvm_caps` list as symbolic `KVM_CAP_*` names and the `optional_features` list. |

All JSON is UTF-8. Write each schema name to the top-level `schema` field. Set `BLK`, `NET`, and `GPU` to integers in `flags`. If `SKIP_LIBKRUNFW` is present, its value must be zero. Populate the library export list from the smolvm loader requirements and the default-path functions checked by `scripts/check-krun-exports.sh`.

## Artifact layout

The library stage contains the three metadata files, the upstream `lib/libkrun.provenance` file, `lib/libkrun.so`, `lib/libkrunfw.so`, `guest/vmlinux`, `guest/kernel.config` and executable `guest/init`. Stage each library's SONAME as a regular file with the same bytes as its base name. The build record at this stage covers both libraries and the three `guest/` files.

The runtime stage produced by smolvm's `scripts/deep/build-bundle.sh --stage runtime` adds these files to the verified library stage.

- Executable `smolvm` wrapper, `smolvm-bin`, `smolvm-resourced` and `guest/agent`.
- `guest/oci-rootfs.tar.gz` with the same executable Agent binary at `usr/local/bin/smolvm-agent` and both rootfs metadata files listed above.

`check-runtime` checks the runtime stage and requires its executables, rootfs and rootfs metadata in the build record. Complete the artifact stage with these files before running `pack`.

- Executable `guest/identity-helper`, recorded in the build record.
- `guest/system-debian.tar.gz` when advertising `system-debian-v1`.
- `profiles/cpu.json`, `profiles/device.json`, `profiles/clock.json` and `profiles/verification.json`. Include the actual CPU template, CPUID/MSR/XCR0 requirements, device layout and clock contract used by the runtime.
- `sources/smolvm.tar.gz`, `sources/libkrun.tar.gz`, `sources/libkrunfw.tar.gz`, `LICENSE` and `NOTICE`.

The packager indexes every regular file and rejects symlinks, special files, empty files and Git LFS pointer contents. Keep writable disks, databases and instance data outside artifact staging.

## Bundle identity

The packager copies staged files into a temporary directory on the output filesystem and checks the copies before publishing the directory. The output must be a new path outside artifact staging. A failed check leaves the destination absent. The static checks cover the kernel configuration, upstream source stamps, build record digests, embedded init bytes, x86-64 ELF headers, static init and identity helper, SONAME copies, libkrun exports, host glibc versions and OCI rootfs Agent bytes.

`runtime-bundle.json` uses `RuntimeBundle/1`. Each artifact has a relative `path`, byte `size` and lowercase hex `sha256`. The bundle digest hashes compact UTF-8 JSON with object keys sorted in Unicode code point order, no insignificant whitespace or trailing newline, and no top-level `bundle_digest` or `signature`. The artifact array is sorted by path. Profile and Guest component digests reference the staged file bytes.

Pass a digest obtained from trusted release metadata to `verify --expect`. Signature authentication, isolated KVM acceptance, and promotion are downstream release steps. Build records and verification profiles record producer claims. The platform tool checks their consistency with the locked inputs and artifacts.

`install` verifies the candidate and its destination copy before atomically publishing a read-only directory. An installation at an existing path succeeds only if that directory is read-only and verifies against the requested digest. Put each new release at a new path and keep its Serve socket, registry and writable VM state outside the installed directory.

For an existing RuntimeBundle/1 that predates both rootfs metadata files, add `--allow-legacy-rootfs` to `verify` or `install` and supply its original digest through `--expect`. Verification still checks the indexed artifacts and the embedded Agent. The exception requires both metadata files to be absent.

```sh
python3 -B tests/integration/test_deep_bundle.py -v
```

## Measure and pack an OCI candidate

Compile the probe inside the locked x86-64 builder. Run the smoke on a Linux KVM Worker with systemd and root access. The probe boots the staged runtime twice using an offline OCI archive. It records KVM extensions and MSR indices on the Worker, and CPUID, XCR0, kernel release, virtio devices and clocksource from inside the guest. The test uses private state and UID claims.

```sh
cc -O2 -static packaging/deep/probe-candidate.c -o "$probe"
sudo python3 -B packaging/deep/smoke-candidate.py \
  --artifacts "$runtime_stage" --probe "$probe" --output "$boot_evidence"
python3 -B packaging/deep/pack-candidate.py \
  --artifacts "$runtime_stage" --evidence "$boot_evidence" \
  --kernel-archive "$kernel_archive" --crun-archive "$crun_archive" --output "$bundle"
```

The packer checks the build lock and build record against the OCI evidence, copies the source and license material, and verifies the final bundle digest. It records the observed OCI boot profile and the exact source reader revision. Deploy with `SMOLVM_AGENT_ROOTFS` pointing to the extracted, verified `guest/oci-rootfs.tar.gz`. Keep the bundle and the independent writable state directory at paths the dropped VMM UID can traverse.

## Measure the managed OCI and Debian System profiles

Use a Debian System ext4 image containing the staged Agent and identity helper. The Worker needs KVM, systemd, debugfs, and a static C compiler for the two Guest probes. The optional `--counter` and `--probe` arguments accept prebuilt copies of those fixtures. Run against a local platformd that supports the managed profile.

```sh
sudo python3 -B packaging/deep/measure-managed.py \
  --artifacts "$runtime_stage" --smolvm "$smolvm_checkout" --platformd "$platformd" \
  --system-image "$system_disk" --output "$managed_evidence"
python3 -B packaging/deep/pack-candidate.py \
  --artifacts "$runtime_stage" --evidence "$boot_evidence" --managed-evidence "$managed_evidence" \
  --kernel-archive "$kernel_archive" --crun-archive "$crun_archive" --output "$bundle"
```

The acceptance record identifies the runtime build, the two test suite commits, platformd and the uncompressed System disk. The OCI cases exercise recursive fork, failed-child retry, rollback, two writable disks and automatic lifecycle. The Debian cases exercise Agent rebinding, Guest grants, and successive durable captures. Verification checks all five logs and the restored disk bytes before accepting the additional contracts.

### Publish measured private networking

Pass `--private-network` to `measure-managed.py` on a Worker with `/dev/net/tun`, nftables and the privileges to create isolated network namespaces. It runs the existing `packet-native` and `packet-fixed` acceptance cases in addition to the five base cases. Each packet log records the observed source and target attachment modes and the shared measurement Bundle digest. Both cases cover TCP reaching a parked execution, exact resume from durable state at the reserved address, and delivery or dropping of the first UDP datagram according to the bound.

The packer selects `RuntimeManagedAcceptance/2` when both directions were measured. The installer verifies each log and reconstructs the measurement manifest from the exact staged runtime, Worker, libraries, Agent and identity helper. It also verifies the measured Debian disk against the packaged archive. The final Bundle digest includes these records and identifies the reader for installed captures.

Serve exposes `network.native`, `network.fixed_internal`, `network.tap` and `network.tap_restore` for the measured Debian managed private-network profile while the configured Worker reports the expected executable digest. These cases certify restore at the same reserved address. `network.native_reidentity` remains unknown pending a release measurement of independent Native identity change. Both acceptance schemas provide the evidence for `lifecycle.standby` and `boot.system_state_restore`. The two packet cases additionally qualify for `lifecycle.standby_wake`. Existing version 1 evidence remains valid for install, verification and historical readers.

## Retain a historical reader and roll back selection

Install each release in its own digest-named directory. Keep a separate Serve process, Unix socket and writable state directory for each release. Configure each verified endpoint under `runtime.custom` with the values from `sandboxctl admin runtime-capabilities`. The Worker broker and its resource catalog are shared. Existing bindings keep their Serve identity and Bundle digest. Switch the selected runtime for new instances to the earlier installed route to roll back new-instance placement. A binary rollback of platformd follows the [platform rollback procedure](../install/README.md) and requires a compatible pre-upgrade database backup for schema changes.

Supply the recorded digest and reader of each retained capture to inspect the installed readers. The matrix checks artifact integrity, the exact reader pair, and any recorded security status. Worker restore still validates its actual StatePoint, CPU and devices.

```sh
python3 packaging/deep/bundle-history.py readers --registry "$registry" \
  --installed "$old_digest" "$old_installation" --installed "$new_digest" "$new_installation" \
  --state "$capture_id" "$capture_bundle" "$capture_reader"
```

Read both catalogs before changing a deployment. A historical binding, retained checkpoint or rescue, unresolved execution, open operation or reachable resource can pin the Bundle. `retire` holds the Platform writer lock and a Worker catalog transaction and moves an unreferenced installation to a separate directory on the same filesystem. It keeps the verified bytes available for a subsequent install. Stop the retiring Serve and platformd before running it.

```sh
python3 packaging/deep/bundle-history.py references --registry "$registry" \
  --worker-catalog "$worker_catalog" --bundle "$old_digest"
python3 packaging/deep/bundle-history.py retire --registry "$registry" \
  --worker-catalog "$worker_catalog" --bundle "$old_digest" \
  --installation "$old_installation" --retired-root "$retired_root"
```

With platformd stopped, an operator can set `active`, `legacy-allowed`, `quarantined` or `revoked` and record a reason. On restart the public Platform excludes quarantined and revoked routes. Their artifacts and references remain available for reviewed recovery in a separate isolated deployment.

```sh
python3 packaging/deep/bundle-history.py set-security --registry "$registry" \
  --bundle "$old_digest" --status quarantined --reason "$review_reason"
```

## Verify the historical-reader workflow on KVM

Use two Bundle revisions with measured OCI launch and StatePoint support, an earlier installed Bundle, platformd and the static Guest workload from the managed runtime benchmark. The fixture isolates its filesystem and network and uses a single temporary Worker catalog for the two managed releases.

```sh
sudo python3 tests/integration/bundle_history_kvm.py --smolvm "$smolvm_checkout" \
  --prior "$prior_bundle" --candidate "$candidate_bundle" --archived "$archived_installation" \
  --platformd "$platformd" --guest "$bench_guest_clean"
```

The fixture checks two independent VMM scopes, one address per slot and exclusive writer authority across Bundles, checkpoint rejection by a different Bundle, both live Serve and archived Serve restart, and old checkpoint recovery after the shared broker restarts and validates the original execution exits. It exercises the Platform security change with the registered archived route. The 2026-11-25 run against the dbe526e archive and the two managed development Bundles recorded results in `.build/wp23/coexistence-kvm.log`. The original platformd binary rejected the upgraded schema, and schema-2 migration retried after an injected final migration failure.
