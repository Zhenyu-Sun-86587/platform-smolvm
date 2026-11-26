#!/usr/bin/env python3
"""Pack a measured OCI candidate with its boot evidence and source licenses."""

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tarfile
import tempfile

import managed_evidence
from bundle import (
    BUILD_LOCK, BUILD_RECORD, PROTOCOLS, ROOTFS, ROOTFS_PROVENANCE, TOOLCHAIN_LOCK,
    check_runtime, pack_bundle, verify_bundle,
)
from bundle_inputs import BundleError, digest, fields, file_digest, match, names, read_json, read_locks, require


KVM_CAPABILITIES = {
    "KVM_CAP_IOEVENTFD", "KVM_CAP_IRQCHIP", "KVM_CAP_IRQFD",
    "KVM_CAP_SET_TSS_ADDR", "KVM_CAP_USER_MEMORY",
}
# libkrun initializes these MSRs before Linux boot and rejects a short write.
BOOT_MSR_INDICES = (
    0x10, 0x174, 0x175, 0x176, 0x1A0, 0x2FF,
    0xC0000081, 0xC0000082, 0xC0000083, 0xC0000084, 0xC0000102,
)


def sha256(path):
    return "sha256:" + file_digest(path)[0]


def write_json(path, contents):
    path.write_text(json.dumps(contents, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def regular_member(archive, path):
    member = archive.getmember(path)
    require(member.isfile() and member.size > 0, f"{path} must be a nonempty regular archive entry")
    with archive.extractfile(member) as stream:
        return stream.read()


def checked_evidence(path, runtime, lock):
    evidence = read_json(path)
    fields(evidence, "schema build_lock_digest build_record_digest boot_count kvm host_cpu guest", "OCI evidence")
    require(evidence["schema"] == "RuntimeOciBootEvidence/1" and evidence["boot_count"] == 2,
            "expected two verified OCI boots")
    for key, artifact in (("build_lock_digest", BUILD_LOCK), ("build_record_digest", BUILD_RECORD)):
        require(evidence[key] == sha256(runtime / artifact), f"OCI evidence has a different {artifact}")
    kvm, cpu, guest = (evidence[name] for name in ("kvm", "host_cpu", "guest"))
    fields(kvm, "version extensions msr_indices", "KVM evidence")
    fields(cpu, "vendor fingerprint", "host CPU evidence")
    fields(guest, "kernel_release clocksource xcr0 cpuid virtio_devices", "guest evidence")
    digest(cpu["fingerprint"], "host CPU fingerprint", prefixed=False)
    require(cpu["vendor"] in {"AuthenticAMD", "GenuineIntel"}, "unknown x86-64 CPU vendor")
    require(kvm["version"] == 12 and set(kvm["extensions"]) == KVM_CAPABILITIES
            and all(type(value) is int and value > 0 for value in kvm["extensions"].values()),
            "KVM lacks the required x86-64 extensions")
    require(guest["kernel_release"] == lock["kernel"]["release"], "OCI evidence has a different kernel release")
    names(kvm["msr_indices"], r"[0-9a-f]{8}", "KVM host MSR indices", required=True)
    match(guest["xcr0"], r"[0-9a-f]{16}", "guest XCR0")
    match(guest["clocksource"], r"[A-Za-z0-9_-]+", "guest clocksource")
    require(isinstance(guest["cpuid"], list) and guest["cpuid"]
            and isinstance(guest["virtio_devices"], list) and guest["virtio_devices"],
            "missing CPU, devices or clock observations")
    for entry in guest["cpuid"]:
        fields(entry, "leaf subleaf eax ebx ecx edx", "guest CPUID leaf")
        require(type(entry["subleaf"]) is int and entry["subleaf"] >= 0, "invalid CPUID subleaf")
        for register in ("leaf", "eax", "ebx", "ecx", "edx"):
            match(entry[register], r"[0-9a-f]{8}", f"CPUID {register}")
    for device in guest["virtio_devices"]:
        fields(device, "slot device_id", "guest virtio device")
        require(type(device["slot"]) is int and device["slot"] >= 0
                and type(device["device_id"]) is int and device["device_id"] > 0, "invalid virtio device")
    return evidence


def source_licenses(stage, lock, kernel, crun):
    licenses = stage / "licenses"
    licenses.mkdir()
    source_members = {
        "smolvm": {"LICENSE": stage / "LICENSE",
                   "THIRD-PARTY-NOTICES.md": licenses / "smolvm-THIRD-PARTY-NOTICES.md",
                   "patches/crun/build-inputs.json": None, "src/portable_checkpoint.rs": None},
        "libkrun": {"LICENSE": licenses / "libkrun-LICENSE"},
        "libkrunfw": {name: licenses / f"libkrunfw-{name}" for name in (
            "LICENSE-GPL-2.0-only", "LICENSE-LGPL-2.1-only",
        )},
    }
    smolvm = {}
    for repository, files in source_members.items():
        with tarfile.open(stage / "sources" / f"{repository}.tar.gz", "r:gz") as archive:
            require(archive.pax_headers.get("comment") == lock["sources"][repository]["commit"],
                    f"{repository} source archive does not match the build lock")
            for name, target in files.items():
                content = regular_member(archive, name)
                if target is None:
                    smolvm[name] = content
                else:
                    target.write_bytes(content)

    kernel_name = "linux-" + lock["kernel"]["release"]
    kernel_copy = stage / "sources" / f"{kernel_name}.tar.xz"
    shutil.copyfile(kernel, kernel_copy)
    require(sha256(kernel_copy) == lock["kernel"]["archive_sha256"], "kernel archive differs from the build lock")
    crun_input = json.loads(smolvm["patches/crun/build-inputs.json"])
    crun_name = "crun-" + match(crun_input["version"], r"[0-9]+(?:\.[0-9]+)+", "crun source version")
    crun_copy = stage / "sources" / f"{crun_name}.tar.gz"
    shutil.copyfile(crun, crun_copy)
    require(file_digest(crun_copy)[0] == crun_input["source"]["sha256"], "crun archive differs from the pinned source")
    with tarfile.open(crun_copy, "r:gz") as archive:
        for name in ("COPYING", "COPYING.libcrun"):
            (licenses / f"crun-{name}").write_bytes(regular_member(archive, f"{crun_name}/{name}"))

    with tarfile.open(stage / ROOTFS, "r:gz") as archive:
        members = [member.name for member in archive if PurePosixPath(member.name).as_posix() == "lib/apk/db/installed"]
        require(len(members) == 1, "OCI template must contain one Alpine package database")
        database = regular_member(archive, members[0])
    package_digest = read_json(stage / ROOTFS_PROVENANCE)["package_db_sha256"]
    require("sha256:" + hashlib.sha256(database).hexdigest() == package_digest, "Alpine package database differs from its provenance")
    records = []
    for package in database.decode().split("\n\n"):
        if not package.strip():
            continue
        values = dict(line.split(":", 1) for line in package.splitlines() if ":" in line)
        require(values.get("P") and values.get("V") and values.get("L"), "Alpine package lacks license metadata")
        records.append(f"{values['P']}\t{values['V']}\t{values['L']}")
    require(records, "OCI template contains no Alpine package records")
    (licenses / "alpine-packages.tsv").write_text("name\tversion\tlicense\n" + "\n".join(sorted(records)) + "\n")
    (stage / "NOTICE").write_text(
        "This Linux x86-64 Runtime Bundle includes smolvm, libkrun, libkrunfw, Linux, "
        "and an Alpine guest template containing crun and crane.\n\n"
        "The smolvm license is in LICENSE. The licenses directory contains the upstream notices, "
        "libkrun, libkrunfw and crun license texts, and the Alpine package license inventory. "
        "The sources directory contains the pinned source archives and patches for the runtime stack.\n",
        encoding="utf-8",
    )
    reader = re.search(rb'^pub const RUNTIME_ABI: &str = "([a-z0-9-]+)";$',
                       smolvm["src/portable_checkpoint.rs"], re.M)
    require(reader is not None, "cannot identify the pinned portable checkpoint reader")
    return reader[1].decode()


def write_profiles(stage, lock, evidence, reader, managed=False, private_network=False, live_resources=False):
    profiles = stage / "profiles"
    profiles.mkdir()
    guest, cpu, kvm = (evidence[name] for name in ("guest", "host_cpu", "kvm"))
    template = "intel-portable" if cpu["vendor"] == "GenuineIntel" else "host-kvm"
    write_json(profiles / "cpu.json", {
        "schema": "RuntimeCpuProfile/1", "architecture": "x86_64",
        "launcher_template_request": 1, "observed_template": template, "host": cpu,
        "guest_cpuid": guest["cpuid"], "guest_xcr0": guest["xcr0"],
        "required_boot_msr_indices": [f"{index:08x}" for index in BOOT_MSR_INDICES],
        "kvm_host_msr_indices": sorted(kvm["msr_indices"]),
        "serialized_msr_filter": "libkrun/src/arch/src/x86_64/linux/msr.rs",
        "checkpoint_compatibility": "exact-host-cpu" if template == "host-kvm" else "linux-kvm-intel-portable-v1",
    })
    write_json(profiles / "device.json", {
        "schema": "RuntimeDeviceProfile/1", "boot_profile": "oci-v1",
        "virtio_devices": guest["virtio_devices"], "kernel_release": guest["kernel_release"],
        "vcpu_count": 2, "memory_mib": 512,
    })
    write_json(profiles / "clock.json", {
        "schema": "RuntimeClockContract/1", "guest_clocksource": guest["clocksource"],
        "x86_vm_state_ioctls": ["KVM_GET_CLOCK", "KVM_SET_CLOCK"],
        "tsc_migration_template": "intel-portable", "verified_operations": ["oci-boot", *(["managed-fork", "hibernate-restore"] if managed else [])],
    })
    sources = lock["sources"]
    contracts = {name: 1 for name in (*PROTOCOLS, *(managed_evidence.MANAGED if managed else ()))}
    if private_network:
        contracts[managed_evidence.PRIVATE_NETWORK] = 1
    if live_resources:
        contracts[managed_evidence.LIVE_RESOURCES] = 1
    write_json(profiles / "verification.json", {
        "schema": "RuntimeVerificationProfile/1", "boot_profiles": ["oci-v1", *(["system-debian-v1"] if managed else [])],
        "contracts": contracts,
        "state_reader_id": f"{reader}:{sources['smolvm']['commit']}:{sources['libkrun']['commit']}",
        "required_kvm_caps": sorted(KVM_CAPABILITIES), "optional_features": [],
    })
    write_json(stage / "metadata/oci-boot-evidence.json", evidence)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("artifacts", "evidence", "kernel-archive", "crun-archive", "output"):
        parser.add_argument(f"--{option}", type=Path, required=True)
    parser.add_argument("--managed-evidence", type=Path, help="measured System template and acceptance output from measure-managed.py")
    args = parser.parse_args()
    try:
        runtime = args.artifacts.resolve(strict=True)
        output = args.output.parent.resolve(strict=True) / args.output.name
        require(output != runtime and runtime not in output.parents, "output must be outside the runtime stage")
        check_runtime(runtime)
        with tempfile.TemporaryDirectory(prefix="deep-candidate-", dir=output.parent) as temporary:
            stage = Path(temporary) / "artifacts"
            shutil.copytree(runtime, stage)
            require(read_json(stage / ROOTFS_PROVENANCE)["schema"] == "RuntimeRootfsProvenance/2",
                    "candidate packing requires the measured OCI identity helper")
            lock, _ = read_locks(stage / BUILD_LOCK, stage / TOOLCHAIN_LOCK)
            evidence = checked_evidence(args.evidence, stage, lock)
            reader = source_licenses(stage, lock, args.kernel_archive, args.crun_archive)
            private_network = live_resources = False
            if args.managed_evidence:
                inputs = args.managed_evidence
                measured = read_json(inputs / managed_evidence.EVIDENCE)
                private_network = managed_evidence.uses_private_network(measured)
                live_resources = measured["schema"] == managed_evidence.SCHEMA_V3
                cases = [*managed_evidence.SCENARIOS]
                if private_network:
                    cases.extend(managed_evidence.PACKET_DIRECTIONS)
                if live_resources:
                    cases.extend(managed_evidence.RESOURCE_PROFILES)
                files = [managed_evidence.EVIDENCE, managed_evidence.SYSTEM_TEMPLATE, *(managed_evidence.log_name(case) for case in cases)]
                for name in files:
                    target = stage / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(inputs / name, target)
                with (stage / "NOTICE").open("a", encoding="utf-8") as notice:
                    notice.write("The System template includes a Debian guest disk with installed package copyright files under /usr/share/doc. "
                                 "The managed acceptance record identifies the tested disk, build and KVM fixture results.\n")
            write_profiles(stage, lock, evidence, reader, bool(args.managed_evidence), private_network, live_resources)
            checksum = pack_bundle(stage, output)
        require(verify_bundle(output, expected=checksum) == checksum, "candidate digest verification failed")
        print(checksum)
    except (BundleError, OSError, ValueError, KeyError, tarfile.TarError) as error:
        sys.exit(f"deep candidate: {error}")


if __name__ == "__main__":
    main()
