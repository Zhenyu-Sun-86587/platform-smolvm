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

For an existing RuntimeBundle/1 that predates both rootfs metadata files, add `verify --allow-legacy-rootfs` and supply its original digest through `--expect`. Verification still checks the indexed artifacts and the embedded Agent. The legacy exception applies only to verification and requires both metadata files to be absent.

```sh
python3 -B tests/integration/test_deep_bundle.py -v
```
