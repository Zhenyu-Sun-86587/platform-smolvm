#!/usr/bin/env python3
"""Measure managed OCI and Debian capabilities on a local KVM Worker."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

import managed_evidence
from bundle import BUILD_LOCK, BUILD_RECORD, IDENTITY_HELPER, ROOTFS, AGENT, PROTOCOLS, check_runtime
from bundle_inputs import BundleError, canonical_json, file_digest, require


def checksum(path):
    return "sha256:" + file_digest(path)[0]


def acceptance_revision(tree, names):
    git = ["git", "-c", f"safe.directory={tree}", "-C", str(tree)]
    revision = subprocess.check_output([*git, "rev-parse", "--verify", "HEAD"], text=True).strip()
    for name in names:
        expected = subprocess.check_output([*git, "show", f"{revision}:{name}"])
        require((tree / name).read_bytes() == expected, f"acceptance fixture has local edits: {name}")
    return revision


def checked_guest(disk, runtime, temporary):
    for guest, name in (("/usr/local/bin/smolvm-agent", AGENT), ("/sbin/smolvm-identity-helper", IDENTITY_HELPER)):
        # All names passed through debugfs are private local constants.
        path = temporary / Path(name).name
        subprocess.run(["debugfs", "-R", f"dump {guest} {path.name}", disk], cwd=temporary, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        require(path.is_file() and checksum(path) == checksum(runtime / name),
                f"Debian image has a different {guest}")


def test_bundle(runtime, dest):
    names = ["smolvm-bin", "smolvm-resourced", "lib/libkrun.so.2", "lib/libkrunfw.so.5", ROOTFS, IDENTITY_HELPER]
    entries = []
    for name in names:
        src = runtime / name
        target = dest / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, target)
        sha, size = file_digest(target)
        entries.append({"path": name, "size": size, "sha256": sha})
    for link, target in (("libkrun.so", "libkrun.so.2"), ("libkrunfw.so", "libkrunfw.so.5")):
        (dest / "lib" / link).symlink_to(target)
    manifest = {"schema": "RuntimeBundle/1", "platform": "linux-x86_64", "artifacts": entries,
                "guest": {"boot_profiles": ["oci-v1", "system-debian-v1"]},
                "contracts": {name: 1 for name in (*PROTOCOLS, *managed_evidence.MANAGED)}}
    manifest["contracts"]["state_reader_id"] = "managed-acceptance:" + checksum(runtime / BUILD_RECORD)
    manifest["bundle_digest"] = "sha256:" + hashlib.sha256(canonical_json(manifest)).hexdigest()
    (dest / "runtime-bundle.json").write_bytes(canonical_json(manifest))


def archive_system(disk, output):
    with output.open("xb") as file, gzip.GzipFile(filename="", fileobj=file, mode="wb", mtime=0, compresslevel=2) as zipped:
        with tarfile.open(mode="w|", fileobj=zipped) as archive, disk.open("rb") as source:
            entry = tarfile.TarInfo(managed_evidence.SYSTEM_DISK)
            entry.size = os.fstat(source.fileno()).st_size
            entry.mode = 0o644
            archive.addfile(entry, source)


def measure(args):
    require(os.geteuid() == 0, "managed acceptance requires KVM and a privileged Worker")
    runtime, image = args.artifacts.resolve(strict=True), args.system_image.resolve(strict=True)
    check_runtime(runtime)
    source = args.smolvm.resolve(strict=True)
    platform = Path(__file__).resolve().parents[2]
    harness = source / "tests/test_deep_managed_launch.py"
    system = platform / "tests/integration/managed_system_vsock.py"
    revisions = {
        "smolvm": acceptance_revision(source, [harness.relative_to(source), "tests/test_deep_native_standby.py",
            "tests/managed_lifecycle_checks.py", "crates/smolvm-resourced/tests/kvm-generation.py",
            "crates/smolvm-resourced/tests/kvm-parked-block-guest.c"]),
        "platform": acceptance_revision(platform, [system.relative_to(platform), "tests/fixtures/vsock-probe.c"]),
    }
    require(0 < image.stat().st_size <= 64 * 1024**3, "invalid Debian root disk size")
    output = args.output.resolve()
    output.mkdir()
    (output / managed_evidence.EVIDENCE).parent.mkdir()
    (output / managed_evidence.SYSTEM_TEMPLATE).parent.mkdir()
    (output / managed_evidence.log_name("oci-capture")).parent.mkdir()
    runs = {}
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="managed-acceptance-") as temp:
        temp = Path(temp)
        checked_guest(image, runtime, temp)
        bundle = temp / "bundle"
        test_bundle(runtime, bundle)
        platformd = args.platformd.resolve(strict=True)
        vm = [sys.executable, str(harness), "--bundle", str(bundle)]
        if args.counter:
            vm.extend(["--guest", str(args.counter.resolve(strict=True))])
        product = [sys.executable, str(system), "--bundle", str(bundle), "--system-image", str(image),
                   "--platformd", str(platformd)]
        if args.probe:
            product.extend(["--probe", str(args.probe.resolve(strict=True))])
        cases = {"oci-capture": (harness, [*vm, "--fork-faults", "--statepoint"]),
                 "lifecycle": (harness, [*vm, "--lifecycle"]),
                 "system-fork": (system, [*product, "--case", "fork"]),
                 "system-captures": (system, [*product, "--case", "successive-captures"]),
                 "system-relay": (system, [*product, "--case", "relay"])}
        for case, (fixture, command) in cases.items():
            path = output / managed_evidence.log_name(case)
            with path.open("wb") as log:
                result = subprocess.run(command, env=env, stdout=log, stderr=subprocess.STDOUT)
            require(result.returncode == 0, f"{case} failed, see {path}")
            managed_evidence.verify_run(case, path)
            runs[case] = {"log_sha256": checksum(path), "harness_sha256": checksum(fixture)}
        archive_system(image, output / managed_evidence.SYSTEM_TEMPLATE)
        evidence = {"schema": "RuntimeManagedAcceptance/1", "build_lock_sha256": checksum(runtime / BUILD_LOCK),
                    "build_record_sha256": checksum(runtime / BUILD_RECORD), "platformd_sha256": checksum(platformd),
                    "system_disk_bytes": image.stat().st_size, "system_disk_sha256": checksum(image),
                    "acceptance_revisions": revisions, "runs": runs}
        (output / managed_evidence.EVIDENCE).write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("artifacts", "smolvm", "platformd", "system-image", "output"):
        parser.add_argument("--" + flag, required=True, type=Path)
    parser.add_argument("--counter", type=Path, help="precompiled static kvm-parked-block-guest fixture")
    parser.add_argument("--probe", type=Path, help="precompiled static vsock-probe fixture")
    try:
        measure(parser.parse_args())
    except (BundleError, OSError, subprocess.SubprocessError, ValueError) as error:
        sys.exit(f"managed acceptance: {error}")
