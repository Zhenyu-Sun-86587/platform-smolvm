#!/usr/bin/env python3
"""Preflight a deep runtime build and pack, verify or install its output."""

import argparse
import hashlib
import mmap
import os
import posixpath
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

from bundle_inputs import (
    BundleError,
    REPOSITORIES,
    canonical_json,
    check_inputs,
    check_toolchain,
    digest,
    fields,
    file_digest,
    match,
    names,
    read_json,
    read_locks,
    relative,
    require,
)


MANIFEST = "runtime-bundle.json"
BUILD_LOCK = "metadata/build-lock.json"
TOOLCHAIN_LOCK = "metadata/toolchain-lock.json"
BUILD_RECORD = "metadata/build-record.json"
VERIFICATION_PROFILE = "profiles/verification.json"
PROVENANCE = "lib/libkrun.provenance"
LIBKRUN = "lib/libkrun.so"
FIRMWARE = "lib/libkrunfw.so"
ROOTFS = "guest/oci-rootfs.tar.gz"
ROOTFS_INPUT_LOCK = "metadata/rootfs-input-lock.json"
ROOTFS_PROVENANCE = "metadata/rootfs-provenance.json"
ROOTFS_METADATA = {ROOTFS_INPUT_LOCK, ROOTFS_PROVENANCE}
ROOTFS_RECIPE = "scripts/build-agent-rootfs.sh"
ROOTFS_INPUT_DIGESTS = ("seed_sha256", "crun_build_sha256", "package_db_sha256", "crane_sha256")
AGENT = "guest/agent"
INIT = "guest/init"
IDENTITY_HELPER = "guest/identity-helper"
CONFIG = "guest/kernel.config"
KERNEL = "guest/vmlinux"
BOOT_TEMPLATES = {"oci-v1": ROOTFS, "system-debian-v1": "guest/system-debian.tar.gz"}
PROTOCOLS = ("worker_resource", "capture_control", "statepoint", "guest_identity", "network_attachment", "block_attachment")
PROFILES = {
    "cpu_profile_digest": "profiles/cpu.json",
    "device_profile_digest": "profiles/device.json",
    "clock_contract_digest": "profiles/clock.json",
}
RUNTIME_EXECUTABLES = ("smolvm", "smolvm-bin", "smolvm-resourced", INIT, AGENT)
EXECUTABLES = (*RUNTIME_EXECUTABLES, IDENTITY_HELPER)
LIBRARY_OUTPUTS = {LIBKRUN, FIRMWARE, KERNEL, CONFIG, INIT}
LIBRARY_FILES = {BUILD_LOCK, TOOLCHAIN_LOCK, BUILD_RECORD, PROVENANCE, *LIBRARY_OUTPUTS}
RUNTIME_OUTPUTS = {*LIBRARY_OUTPUTS, ROOTFS, *ROOTFS_METADATA, *RUNTIME_EXECUTABLES}
RUNTIME_FILES = {*LIBRARY_FILES, *RUNTIME_OUTPUTS}
REQUIRED_FILES = {
    *RUNTIME_FILES, VERIFICATION_PROFILE, "LICENSE", "NOTICE",
    *EXECUTABLES, *PROFILES.values(), *(f"sources/{repo}.tar.gz" for repo in REPOSITORIES),
}


def manifest_digest(manifest):
    payload = {key: value for key, value in manifest.items() if key not in {"bundle_digest", "signature"}}
    return "sha256:" + hashlib.sha256(canonical_json(payload)).hexdigest()


def list_files(root):
    require(stat.S_ISDIR(root.lstat().st_mode), f"{root} must be a directory")
    files = {}
    directories = [root]
    while directories:
        for path in directories.pop().iterdir():
            name = relative(path.relative_to(root).as_posix(), "artifact path")
            mode = path.lstat().st_mode
            if stat.S_ISDIR(mode):
                directories.append(path)
            else:
                require(stat.S_ISREG(mode), f"bundle entries must be regular files: {name}")
                files[name] = mode
    return files


def artifact_index(root, files):
    artifacts = {}
    for name in sorted(files):
        checksum, size = file_digest(root / name)
        artifacts[name] = {"path": name, "size": size, "sha256": checksum}
    return artifacts


def read_profile(root):
    profile = read_json(root / VERIFICATION_PROFILE)
    fields(profile, "schema boot_profiles contracts state_reader_id required_kvm_caps optional_features", "verification profile")
    require(profile["schema"] == "RuntimeVerificationProfile/1", "unsupported verification profile schema")
    boot_profiles = names(profile["boot_profiles"], r"[a-z0-9-]+", "boot_profiles", required=True)
    require("oci-v1" in boot_profiles and set(boot_profiles) <= BOOT_TEMPLATES.keys(), "unsupported boot profiles")
    fields(profile["contracts"], " ".join(PROTOCOLS), "contracts")
    for name, version in profile["contracts"].items():
        require(type(version) is int and version == 1, f"unsupported contract version: {name}")
    match(profile["state_reader_id"], r"[A-Za-z0-9][A-Za-z0-9._:+/-]*", "state_reader_id")
    names(profile["required_kvm_caps"], r"KVM_CAP_[A-Z0-9_]+", "required_kvm_caps", required=True)
    names(profile["optional_features"], r"[A-Za-z0-9][A-Za-z0-9._-]*", "optional_features")
    for name in PROFILES.values():
        contract = read_json(root / name)
        require(isinstance(contract, dict) and contract, f"{name} must contain the runtime's contract")
    return profile


def verify_config(path, expected):
    actual = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        assignment = re.fullmatch(r"(CONFIG_[A-Z0-9_]+)=(.*)", line)
        disabled = re.fullmatch(r"# (CONFIG_[A-Z0-9_]+) is not set", line)
        if assignment or disabled:
            name = (assignment or disabled)[1]
            require(name not in actual, f"duplicate kernel config entry: {name}")
            actual[name] = assignment[2] if assignment else "n"
    for name, value in expected.items():
        require(actual.get(name) == value, f"kernel config requires {name}={value}")


def verify_provenance(root, lock, artifacts, required):
    recorded = {}
    for line in (root / PROVENANCE).read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            name, sep, commit = line.partition("=")
            require(sep and name not in recorded, "invalid libkrun provenance record")
            recorded[name] = commit
    expected = {name: lock["sources"][name]["commit"] for name in ("libkrun", "libkrunfw")}
    require(recorded == expected, "libkrun provenance does not match the pinned source commits")

    record = read_json(root / BUILD_RECORD)
    fields(record, "schema build_lock_digest outputs", "build record")
    require(record["schema"] == "RuntimeBuildRecord/1", "unsupported build record schema")
    require(
        record["build_lock_digest"] == "sha256:" + artifacts[BUILD_LOCK]["sha256"],
        "build record belongs to a different build lock",
    )
    outputs = record["outputs"]
    require(isinstance(outputs, dict), "build record outputs must be an object")
    require(required <= outputs.keys(), "build record is missing required output digests")
    for name, checksum in outputs.items():
        require(name in artifacts and name != BUILD_RECORD, f"invalid build record output path: {name}")
        require(checksum == "sha256:" + artifacts[name]["sha256"], f"build record output digest mismatch: {name}")

    with (root / LIBKRUN).open("rb") as library:
        with mmap.mmap(library.fileno(), 0, access=mmap.ACCESS_READ) as mapped:
            require(mapped.find((root / INIT).read_bytes()) >= 0, "libkrun has a different embedded init")


def elf(path, kind, *, static=False):
    result = subprocess.run(
        ["readelf", "-W", "-h", "-l", "-d", "--dyn-syms", "--version-info", str(path)],
        capture_output=True, text=True, env={**os.environ, "LC_ALL": "C"},
    )
    require(result.returncode == 0 and not result.stderr, f"readelf failed for {path}: {result.stderr.strip()}")
    data = result.stdout
    require(re.search(r"^\s*Class:\s+ELF64\s*$", data, re.M) and
            re.search(r"^\s*Data:.*little endian\s*$", data, re.M) and
            re.search(r"^\s*Machine:\s+Advanced Micro Devices X86-64\s*$", data, re.M),
            f"{path} must be an x86-64 ELF")
    elf_type = re.search(r"^\s*Type:\s+(\S+)", data, re.M)
    require(elf_type and elf_type[1] in kind, f"{path} has an unexpected ELF type")
    require(re.search(r"^\s*LOAD\s", data, re.M), f"{path} has no loadable ELF segments")
    if kind != {"DYN"}:
        entry = re.search(r"^\s*Entry point address:\s+(0x[0-9a-f]+)", data, re.M)
        require(entry and int(entry[1], 16) > 0, f"{path} has no executable entry point")
    if static:
        require(not re.search(r"^\s*INTERP\s|\(NEEDED\)", data, re.M), f"{path} must be statically linked")
    return data


def check_elf_artifacts(root, lock, artifacts, executables):
    elf(root / INIT, {"EXEC", "DYN"}, static=True)
    if IDENTITY_HELPER in executables:
        elf(root / IDENTITY_HELPER, {"EXEC", "DYN"}, static=True)
    if AGENT in executables:
        elf(root / AGENT, {"EXEC", "DYN"})
    host = [root / name for name in ("smolvm-bin", "smolvm-resourced") if name in executables]
    maximum = glibc_version(lock["maximum_glibc"])
    for path in host + [root / LIBKRUN, root / FIRMWARE]:
        library = path.parent == root / "lib"
        data = elf(path, {"DYN"} if library else {"EXEC", "DYN"})
        needs = data.partition("Version needs section")[2]
        for version in re.findall(r"\bName:\s+GLIBC_(\S+)", needs):
            match(version, r"[0-9]+\.[0-9]+(?:\.[0-9]+)?", f"{path} GLIBC requirement")
            require(glibc_version(version) <= maximum, f"{path} needs GLIBC_{version}, above the build lock")
        required_libraries = re.findall(r"\(NEEDED\).*?\[([^\]]+)\]", data)
        require(
            not any(name.startswith(("libvirglrenderer", "libepoxy", "libvulkan")) for name in required_libraries),
            f"{path} has a host GPU dependency",
        )
        if library:
            soname = re.search(r"\(SONAME\).*?\[([^\]]+)\]", data)
            require(soname is not None, f"{path} has no SONAME")
            alias = artifacts.get("lib/" + relative(soname[1], "library SONAME"))
            require(alias and alias["sha256"] == artifacts["lib/" + path.name]["sha256"],
                    f"{path} is missing the matching SONAME file {soname[1]}")
        if path == root / LIBKRUN:
            exports = set(re.findall(
                r"^\s*\d+:\s+\S+\s+\d+\s+(?:FUNC|IFUNC)\s+(?:GLOBAL|WEAK)\s+"
                r"(?:DEFAULT|PROTECTED)\s+\d+\s+(krun_[a-zA-Z0-9_]+)(?:@\S+)?(?:\s|$)",
                data, re.M,
            ))
            missing = sorted(set(lock["required_libkrun_exports"]) - exports)
            require(not missing, f"libkrun is missing required exports: {', '.join(missing)}")


def glibc_version(value):
    major, minor, patch, *_ = value.split(".") + ["0"]
    return int(major), int(minor), int(patch)


def verify_rootfs(root, artifacts, *, measured_helper):
    programs = {"usr/local/bin/smolvm-agent": AGENT}
    if measured_helper:
        programs["sbin/smolvm-identity-helper"] = IDENTITY_HELPER
    found = set()
    with tarfile.open(root / ROOTFS, "r|gz") as archive:
        for member in archive:
            path = posixpath.normpath(member.name).lstrip("/")
            if path not in programs:
                continue
            expected = artifacts[programs[path]]
            name = posixpath.basename(path)
            require(
                path not in found and not member.name.startswith("/") and ".." not in member.name.split("/")
                and member.isfile() and member.mode & 0o111 and member.size == expected["size"],
                f"OCI rootfs must contain one executable {name} matching the bundle",
            )
            checksum = hashlib.sha256()
            with archive.extractfile(member) as handle:
                while chunk := handle.read(1024 * 1024):
                    checksum.update(chunk)
            require(checksum.hexdigest() == expected["sha256"], f"OCI rootfs has a different {name}")
            found.add(path)
    for path in programs:
        require(path in found, f"OCI rootfs is missing {posixpath.basename(path)}")


def verify_rootfs_provenance(root, artifacts, toolchain, provenance):
    seed = read_json(root / ROOTFS_INPUT_LOCK)
    fields(seed, "schema recipe " + " ".join(ROOTFS_INPUT_DIGESTS), "rootfs input lock")
    require(seed["schema"] == "RuntimeRootfsSeed/1", "unsupported rootfs input lock schema")
    require(seed["recipe"] == ROOTFS_RECIPE, "unsupported rootfs build recipe")

    require(provenance.get("schema") in ("RuntimeRootfsProvenance/1", "RuntimeRootfsProvenance/2"),
            "unsupported rootfs provenance schema")
    measured = provenance["schema"] == "RuntimeRootfsProvenance/2"
    fields(
        provenance, "schema input_lock_path input_lock_sha256 recipe_sha256 agent_sha256 rootfs_sha256 "
        + ("identity_helper_sha256 " if measured else "") + " ".join(ROOTFS_INPUT_DIGESTS), "rootfs provenance",
    )
    input_path = relative(provenance["input_lock_path"], "rootfs input lock source path")
    pins = {entry["path"]: entry["sha256"] for entry in toolchain["lockfiles"] if entry["repository"] == "smolvm"}
    require(measured == ("crates/smolvm-identity-helper/Cargo.toml" in pins),
            "rootfs helper measurement must match the toolchain lock")
    require(pins.get(input_path) == provenance["input_lock_sha256"], "rootfs input lock is not pinned by the toolchain lock")
    recipe = digest(provenance["recipe_sha256"], "rootfs recipe SHA-256")
    if ROOTFS_RECIPE in pins:
        require(pins[ROOTFS_RECIPE] == recipe, "rootfs recipe digest differs from the toolchain lock")
    for name in ROOTFS_INPUT_DIGESTS:
        value = digest(seed[name], f"rootfs input lock {name}")
        require(provenance[name] == value, f"rootfs provenance {name} differs from the input lock")
    binaries = {"input_lock_sha256": ROOTFS_INPUT_LOCK, "agent_sha256": AGENT, "rootfs_sha256": ROOTFS}
    if measured:
        binaries["identity_helper_sha256"] = IDENTITY_HELPER
    for key, path in binaries.items():
        require(provenance[key] == "sha256:" + artifacts[path]["sha256"], f"rootfs provenance {key} does not match {path}")


def checked_artifacts(root, files, required, outputs, executables):
    metadata = read_json(root / ROOTFS_PROVENANCE) if ROOTFS_PROVENANCE in outputs and ROOTFS_PROVENANCE in files else {}
    require(isinstance(metadata, dict), "rootfs provenance must be an object")
    measured = metadata.get("schema") == "RuntimeRootfsProvenance/2"
    if measured:
        required = required | {IDENTITY_HELPER}
        outputs = outputs | {IDENTITY_HELPER}
        executables = set(executables) | {IDENTITY_HELPER}
    missing = sorted(required - files.keys())
    require(not missing, f"missing required bundle artifacts: {', '.join(missing)}")
    for name in executables:
        require(files[name] & stat.S_IXUSR, f"bundle executable is missing its executable mode: {name}")
    build, toolchain = read_locks(root / BUILD_LOCK, root / TOOLCHAIN_LOCK)
    artifacts = artifact_index(root, files)

    verify_config(root / CONFIG, build["kernel"]["required_config"])
    verify_provenance(root, build, artifacts, outputs)
    check_elf_artifacts(root, build, artifacts, executables)
    if ROOTFS in outputs:
        verify_rootfs(root, artifacts, measured_helper=measured)
    if ROOTFS_METADATA <= outputs:
        verify_rootfs_provenance(root, artifacts, toolchain, metadata)
    return build, artifacts


def build_manifest(root, files, *, allow_legacy_rootfs=False):
    required = REQUIRED_FILES
    outputs = {*RUNTIME_OUTPUTS, IDENTITY_HELPER}
    if allow_legacy_rootfs and not ROOTFS_METADATA.intersection(files):
        required = required - ROOTFS_METADATA
        outputs -= ROOTFS_METADATA
    build, artifacts = checked_artifacts(root, files, required, outputs, EXECUTABLES)
    profile = read_profile(root)
    for name in profile["boot_profiles"]:
        require(BOOT_TEMPLATES[name] in files, f"missing template for {name}")

    def artifact_digest(name):
        return "sha256:" + artifacts[name]["sha256"]

    manifest = {
        "schema": "RuntimeBundle/1",
        "platform": "linux-x86_64",
        "sources": build["sources"],
        "guest": {
            "kernel_release": build["kernel"]["release"],
            "kernel_digest": artifact_digest(KERNEL),
            "config_digest": artifact_digest(CONFIG),
            "init_digest": artifact_digest(INIT),
            "agent_digest": artifact_digest(AGENT),
            "identity_helper_digest": artifact_digest(IDENTITY_HELPER),
            "boot_profiles": profile["boot_profiles"],
        },
        "build": {
            "toolchain_lock_digest": build["toolchain_lock_digest"],
            "flags": build["flags"],
            "patch_ids": [patch["id"] for patch in build["patches"]],
        },
        "artifacts": list(artifacts.values()),
        "contracts": {
            **profile["contracts"],
            "state_reader_id": profile["state_reader_id"],
            **{label: artifact_digest(name) for label, name in PROFILES.items()},
        },
        "host_requirements": {
            "os": "linux",
            "arch": "x86_64",
            "kvm_required": True,
            "required_kvm_caps": profile["required_kvm_caps"],
            "optional_features": profile["optional_features"],
        },
        "verification_profile_digest": artifact_digest(VERIFICATION_PROFILE),
    }
    manifest["bundle_digest"] = manifest_digest(manifest)
    return manifest


def check_libraries(root):
    checked_artifacts(root, list_files(root), LIBRARY_FILES, LIBRARY_OUTPUTS, (INIT,))


def check_runtime(root):
    checked_artifacts(root, list_files(root), RUNTIME_FILES, RUNTIME_OUTPUTS, RUNTIME_EXECUTABLES)


def verify_bundle(root, expected=None, *, allow_legacy_rootfs=False):
    files = list_files(root)
    require(MANIFEST in files, f"missing {MANIFEST}")
    del files[MANIFEST]
    manifest = read_json(root / MANIFEST)
    require(isinstance(manifest, dict) and manifest.get("schema") == "RuntimeBundle/1", "unsupported bundle schema")
    actual_digest = digest(manifest.get("bundle_digest"), "bundle_digest")
    require(actual_digest == manifest_digest(manifest), "bundle manifest digest mismatch")
    if expected is not None:
        require(actual_digest == digest(expected, "expected bundle digest"), "unexpected bundle digest")

    entries = manifest.get("artifacts")
    require(isinstance(entries, list), "artifacts must be an array")
    indexed = set()
    for entry in entries:
        fields(entry, "path size sha256", "artifact")
        name = relative(entry["path"], "artifact path")
        require(name not in indexed, f"duplicate artifact path: {name}")
        indexed.add(name)
        require(type(entry["size"]) is int and entry["size"] > 0, f"invalid artifact size: {name}")
        digest(entry["sha256"], f"{name} SHA-256", prefixed=False)
        require(name in files, f"missing artifact: {name}")
        info = (root / name).stat()
        require(info.st_size == entry["size"], f"artifact size mismatch: {name}")
        require(info.st_nlink == 1, f"hardlinked bundle artifact: {name}")
    require(indexed == files.keys(), f"unindexed bundle artifacts: {', '.join(sorted(files.keys() - indexed))}")

    actual = build_manifest(root, files, allow_legacy_rootfs=allow_legacy_rootfs)
    if actual["bundle_digest"] != actual_digest:
        for actual_entry, expected_entry in zip(actual["artifacts"], entries):
            require(actual_entry == expected_entry, f"artifact digest or order mismatch: {expected_entry['path']}")
        raise BundleError("bundle manifest fields do not match the locked build and artifacts")
    return actual_digest


def copy_bundle_files(source, candidate, files):
    for name, mode in files.items():
        path = candidate / name
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source / name, path)
        path.chmod(0o555 if mode & stat.S_IXUSR else 0o444)


def pack_bundle(staging, output):
    files = list_files(staging)
    require(MANIFEST not in files, "staged outputs already contain a bundle manifest")
    staging = staging.resolve(strict=True)
    output = output.parent.resolve(strict=True) / output.name
    require(output != staging and staging not in output.parents, "output must be outside artifact staging")
    require(not output.exists() and not output.is_symlink(), f"output already exists: {output}")
    with tempfile.TemporaryDirectory(prefix=".deep-bundle-", dir=output.parent) as work:
        candidate = Path(work) / "candidate"
        candidate.mkdir()
        copy_bundle_files(staging, candidate, files)
        manifest = build_manifest(candidate, list_files(candidate))
        path = candidate / MANIFEST
        path.write_bytes(canonical_json(manifest) + b"\n")
        path.chmod(0o444)
        subprocess.run(["mv", "-nT", "--", str(candidate), str(output)], check=True)
        require(not candidate.exists(), f"output already exists: {output}")
    return manifest["bundle_digest"]


def sync_path(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def verify_installed_bundle(output, expected, *, allow_legacy_rootfs=False):
    checksum = verify_bundle(output, expected, allow_legacy_rootfs=allow_legacy_rootfs)
    for directory, _, filenames in os.walk(output):
        path = Path(directory)
        require(not path.stat().st_mode & 0o222, f"installed directory is writable: {path}")
        for name in filenames:
            artifact = path / name
            require(not artifact.stat().st_mode & 0o222, f"installed artifact is writable: {artifact}")
    return checksum


def install_bundle(source, output, expected, *, allow_legacy_rootfs=False):
    expected = digest(expected, "expected bundle digest")
    source = source.resolve(strict=True)
    output = output.parent.resolve(strict=True) / output.name
    if output.exists() or output.is_symlink():
        return verify_installed_bundle(output, expected, allow_legacy_rootfs=allow_legacy_rootfs)
    require(source not in output.parents, "installation must be outside the source bundle")
    verify_bundle(source, expected, allow_legacy_rootfs=allow_legacy_rootfs)
    files = list_files(source)
    with tempfile.TemporaryDirectory(prefix=".deep-install-", dir=output.parent) as work:
        candidate = Path(work)
        copy_bundle_files(source, candidate, files)
        verify_bundle(candidate, expected, allow_legacy_rootfs=allow_legacy_rootfs)
        for name in files:
            sync_path(candidate / name)
        directories = [Path(path) for path, _, _ in os.walk(candidate, topdown=False)]
        try:
            for path in directories:
                path.chmod(0o555)
                sync_path(path)
            subprocess.run(["mv", "-nT", "--", str(candidate), str(output)], check=True)
            if candidate.exists():
                return verify_installed_bundle(output, expected, allow_legacy_rootfs=allow_legacy_rootfs)
            sync_path(output.parent)
        finally:
            if candidate.exists():
                for path in reversed(directories):
                    path.chmod(0o755)
    return expected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    check = commands.add_parser("check-inputs", help="verify clean locked sources and the builder before compilation")
    check.add_argument("--build-lock", type=Path, required=True)
    check.add_argument("--toolchain-lock", type=Path, required=True)
    check.add_argument("--smolvm", type=Path, required=True)
    check.add_argument("--kernel-archive", type=Path, required=True)
    check.add_argument("--builder-image", required=True)
    libraries = commands.add_parser("check-libraries", help="check the isolated firmware, libkrun and init outputs")
    libraries.add_argument("--artifacts", type=Path, required=True)
    runtime = commands.add_parser("check-runtime", help="check the runtime executable and OCI rootfs build stage")
    runtime.add_argument("--artifacts", type=Path, required=True)
    pack = commands.add_parser("pack", help="check staged build outputs and assemble a new bundle directory")
    pack.add_argument("--artifacts", type=Path, required=True)
    pack.add_argument("--output", type=Path, required=True)
    verify = commands.add_parser("verify", help="verify a RuntimeBundle/1 directory")
    verify.add_argument("bundle", type=Path)
    verify.add_argument("--expect", help="expected sha256: digest from trusted release metadata")
    verify.add_argument("--allow-legacy-rootfs", action="store_true", help="allow older v1 bundles without rootfs provenance")
    install = commands.add_parser("install", help="verify and atomically install an immutable bundle directory")
    install.add_argument("bundle", type=Path)
    install.add_argument("--expect", required=True, help="expected sha256: digest from trusted release metadata")
    install.add_argument("--output", type=Path, required=True)
    install.add_argument("--allow-legacy-rootfs", action="store_true", help="allow older v1 bundles without rootfs provenance")
    args = parser.parse_args()
    try:
        if args.command == "check-inputs":
            build, toolchain = read_locks(args.build_lock, args.toolchain_lock)
            check_inputs(args.smolvm, args.kernel_archive, build, toolchain)
            check_toolchain(toolchain, args.builder_image, args.smolvm)
            print("Build inputs verified.")
        elif args.command == "check-libraries":
            check_libraries(args.artifacts)
            print("Runtime libraries and init verified.")
        elif args.command == "check-runtime":
            check_runtime(args.artifacts)
            print("Runtime executables and OCI rootfs verified.")
        elif args.command == "pack":
            print(pack_bundle(args.artifacts, args.output))
        elif args.command == "install":
            print(install_bundle(args.bundle, args.output, args.expect, allow_legacy_rootfs=args.allow_legacy_rootfs))
        else:
            print(verify_bundle(args.bundle, args.expect, allow_legacy_rootfs=args.allow_legacy_rootfs))
            if args.allow_legacy_rootfs and not (args.bundle / ROOTFS_PROVENANCE).exists():
                print("Verified the legacy bundle layout with the requested rootfs provenance exception.", file=sys.stderr)
    except (BundleError, OSError, UnicodeError, tarfile.TarError, subprocess.CalledProcessError) as error:
        print(f"deep bundle: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
