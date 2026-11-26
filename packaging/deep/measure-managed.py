#!/usr/bin/env python3
"""Measure managed OCI and Debian capabilities on a local KVM Worker."""
import argparse
import gzip
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

import managed_evidence
from bundle import AGENT, BUILD_LOCK, BUILD_RECORD, IDENTITY_HELPER, MANIFEST, PROTOCOLS, check_runtime
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
    entries = {}
    for name, installed in managed_evidence.MEASURED_ARTIFACTS.items():
        src = runtime / name
        target = dest / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, target)
        sha, size = file_digest(target)
        entries[installed] = {"path": installed, "size": size, "sha256": sha}
    for link, target in (("libkrun.so", "libkrun.so.2"), ("libkrunfw.so", "libkrunfw.so.5")):
        (dest / "lib" / link).symlink_to(target)
    contracts = {name: 1 for name in (*PROTOCOLS, *managed_evidence.MANAGED)}
    manifest = managed_evidence.measurement_manifest(entries, checksum(runtime / BUILD_RECORD), contracts)
    (dest / MANIFEST).write_bytes(canonical_json(manifest))
    return manifest["bundle_digest"]


def archive_system(disk, output):
    with output.open("xb") as file, gzip.GzipFile(filename="", fileobj=file, mode="wb", mtime=0, compresslevel=2) as zipped:
        with tarfile.open(mode="w|", fileobj=zipped) as archive, disk.open("rb") as source:
            entry = tarfile.TarInfo(managed_evidence.SYSTEM_DISK)
            entry.size = os.fstat(source.fileno()).st_size
            entry.mode = 0o644
            archive.addfile(entry, source)


def probe_binary(supplied, source, output, cc):
    if supplied:
        shutil.copy2(supplied.resolve(strict=True), output)
    else:
        subprocess.run([cc, "-static", "-Os", "-o", output, source], check=True)
    return output


def stage_guest(runtime, image, platformd, temporary):
    snapshot = temporary / managed_evidence.SYSTEM_DISK
    subprocess.run(["cp", "--reflink=auto", "--sparse=always", "--", image, snapshot], check=True)
    checked_guest(snapshot, runtime, temporary)
    bundle = temporary / "bundle"
    measured = test_bundle(runtime, bundle)
    binary = Path(shutil.copy2(platformd.resolve(strict=True), temporary / "platformd"))
    return snapshot, bundle, measured, binary


def measure_authorized(args, runtime, image, source, platform):
    fixture = platform / "tests/integration/authorized_services_kvm.py"
    revisions = {
        "smolvm": acceptance_revision(source, ["crates/smolvm-runtime-protocol/src/measured_release.rs"]),
        "platform": acceptance_revision(platform, [fixture.relative_to(platform), "tests/integration/managed_system_vsock.py",
                                                     "tests/fixtures/authorized-service.pl"]),
    }
    output = args.output.resolve()
    output.mkdir()
    (output / managed_evidence.SYSTEM_TEMPLATE).parent.mkdir()
    (output / managed_evidence.log_name("system-boot")).parent.mkdir(parents=True)
    selected = set(args.authorized_case or managed_evidence.AUTHORIZED_CASES) | {"system-boot"}
    runs = {}
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="authorized-acceptance-") as temporary:
        disk, bundle, measured, platformd = stage_guest(runtime, image, args.platformd, Path(temporary))
        product = [sys.executable, "-B", str(fixture), "--bundle", str(bundle), "--system-image", str(disk),
                   "--platformd", str(platformd), "--work-dir", str(args.work_dir)]
        for case, (name, mode) in managed_evidence.AUTHORIZED_CASES.items():
            if case not in selected:
                continue
            path = output / managed_evidence.log_name(case)
            command = [*product, "--case", name]
            if mode:
                command.extend(["--mode", mode])
            with path.open("wb") as log:
                result = subprocess.run(command, env=env, stdout=log, stderr=subprocess.STDOUT)
            require(result.returncode == 0, f"{case} failed, see {path}")
            managed_evidence.verify_authorized_log(case, path, measured)
            runs[case] = {"log_sha256": checksum(path), "harness_sha256": checksum(fixture)}
        template = output / managed_evidence.SYSTEM_TEMPLATE
        archive_system(disk, template)
        evidence = {"schema": managed_evidence.SCHEMA_V4,
                    "build_lock_sha256": checksum(runtime / BUILD_LOCK),
                    "build_record_sha256": checksum(runtime / BUILD_RECORD), "platformd_sha256": checksum(platformd),
                    "system_disk_bytes": disk.stat().st_size, "system_disk_sha256": checksum(disk),
                    "acceptance_revisions": revisions, "measurement_bundle_digest": measured,
                    "system_template_sha256": checksum(template), "runs": runs}
        (output / managed_evidence.EVIDENCE).write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")


def measure(args):
    require(os.geteuid() == 0, "managed acceptance requires KVM and a privileged Worker")
    runtime, image = args.artifacts.resolve(strict=True), args.system_image.resolve(strict=True)
    check_runtime(runtime)
    source = args.smolvm.resolve(strict=True)
    platform = Path(__file__).resolve().parents[2]
    require(0 < image.stat().st_size <= 64 * 1024**3, "invalid Debian root disk size")
    if args.authorized_only:
        measure_authorized(args, runtime, image, source, platform)
        return

    harness = source / "tests/test_deep_managed_launch.py"
    system = platform / "tests/integration/managed_system_vsock.py"
    revisions = {
        "smolvm": acceptance_revision(source, [harness.relative_to(source), "tests/test_deep_native_standby.py",
            "tests/managed_lifecycle_checks.py", "crates/smolvm-resourced/tests/kvm-generation.py",
            "crates/smolvm-resourced/tests/kvm-parked-block-guest.c"]),
        "platform": acceptance_revision(platform, [system.relative_to(platform), "tests/fixtures/vsock-probe.c"]),
    }
    output = args.output.resolve()
    output.mkdir()
    (output / managed_evidence.EVIDENCE).parent.mkdir()
    (output / managed_evidence.SYSTEM_TEMPLATE).parent.mkdir()
    (output / managed_evidence.log_name("oci-capture")).parent.mkdir()
    runs = {}
    network_runs = {}
    resource_runs = {}
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="managed-acceptance-") as temp:
        temp = Path(temp)
        image, bundle, measured_bundle, platformd = stage_guest(runtime, image, args.platformd, temp)
        counter = probe_binary(args.counter, source / "crates/smolvm-resourced/tests/kvm-parked-block-guest.c",
                               temp / "counter", args.cc)
        probe = probe_binary(args.probe, platform / "tests/fixtures/vsock-probe.c", temp / "vsock-probe", args.cc)
        vm = [sys.executable, str(harness), "--bundle", str(bundle), "--guest", str(counter)]
        product = [sys.executable, str(system), "--bundle", str(bundle), "--system-image", str(image),
                   "--platformd", str(platformd), "--probe", str(probe)]
        cases = {"oci-capture": (harness, [*vm, "--fork-faults", "--statepoint"]),
                 "lifecycle": (harness, [*vm, "--lifecycle"]),
                 "system-fork": (system, [*product, "--case", "fork"]),
                 "system-captures": (system, [*product, "--case", "successive-captures"]),
                 "system-relay": (system, [*product, "--case", "relay"])}
        if args.private_network:
            cases.update({case: (system, [*product, "--case", case]) for case in managed_evidence.PACKET_DIRECTIONS})
        if args.live_resources:
            cases.update({case: (system, [*product, "--case", case]) for case in managed_evidence.RESOURCE_PROFILES})
        for case, (fixture, command) in cases.items():
            path = output / managed_evidence.log_name(case)
            with path.open("wb") as log:
                result = subprocess.run(command, env=env, stdout=log, stderr=subprocess.STDOUT)
            require(result.returncode == 0, f"{case} failed, see {path}")
            managed_evidence.verify_run(case, path, measured_bundle)
            run = {"log_sha256": checksum(path), "harness_sha256": checksum(fixture)}
            if case in managed_evidence.PACKET_DIRECTIONS:
                source_mode, target_mode = managed_evidence.PACKET_DIRECTIONS[case]
                network_runs[case] = {**run, "source_mode": source_mode, "target_mode": target_mode}
            elif case in managed_evidence.RESOURCE_PROFILES:
                resource_runs[case] = {**run, "profile": managed_evidence.RESOURCE_PROFILES[case]}
            else:
                runs[case] = run
        template = output / managed_evidence.SYSTEM_TEMPLATE
        archive_system(image, template)
        evidence = {"schema": managed_evidence.SCHEMA_V1, "build_lock_sha256": checksum(runtime / BUILD_LOCK),
                    "build_record_sha256": checksum(runtime / BUILD_RECORD), "platformd_sha256": checksum(platformd),
                    "system_disk_bytes": image.stat().st_size, "system_disk_sha256": checksum(image),
                    "acceptance_revisions": revisions, "counter_sha256": checksum(counter),
                    "vsock_probe_sha256": checksum(probe), "runs": runs}
        if args.private_network:
            evidence["schema"] = managed_evidence.SCHEMA_V2
            evidence["private_network"] = {"measurement_bundle_digest": measured_bundle,
                                           "system_template_sha256": checksum(template), "runs": network_runs}
        if args.live_resources:
            evidence["schema"] = managed_evidence.SCHEMA_V3
            evidence["live_resources"] = {"measurement_bundle_digest": measured_bundle,
                                          "system_template_sha256": checksum(template), "runs": resource_runs}
        (output / managed_evidence.EVIDENCE).write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("artifacts", "smolvm", "platformd", "system-image", "output"):
        parser.add_argument("--" + flag, required=True, type=Path)
    parser.add_argument("--counter", type=Path, help="precompiled static kvm-parked-block-guest fixture")
    parser.add_argument("--probe", type=Path, help="precompiled static vsock-probe fixture")
    parser.add_argument("--private-network", action="store_true", help="also measure both managed private-network directions in KVM")
    parser.add_argument("--live-resources", action="store_true", help="measure public resource adjustments on both System compute profiles in KVM")
    parser.add_argument("--authorized-only", action="store_true", help="measure authorized System and private TCP lifecycle cases")
    parser.add_argument("--authorized-case", action="append", choices=managed_evidence.AUTHORIZED_CASES,
                        help="select an authorized case in addition to System boot; defaults to all authorized cases")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"), help="scratch directory for private measurement inputs")
    parser.add_argument("--cc", default="cc", help="C compiler for Guest test fixtures")
    args = parser.parse_args()
    if args.authorized_only and (args.private_network or args.live_resources or args.probe or args.counter):
        parser.error("--authorized-only has its own fixtures and cases")
    if args.authorized_case and not args.authorized_only:
        parser.error("--authorized-case requires --authorized-only")
    try:
        measure(args)
    except (BundleError, OSError, subprocess.SubprocessError, ValueError) as error:
        sys.exit(f"managed acceptance: {error}")
