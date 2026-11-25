"""Validation shared by the deep build preflight and bundle packager."""

import hashlib
import json
import os
import re
import stat
import subprocess
from pathlib import Path


REPOSITORIES = ("smolvm", "libkrun", "libkrunfw")
DEFAULT_EXPORTS = {"krun_add_disk2", "krun_add_net_unixstream", "krun_set_egress_policy", "krun_create_disk_overlay"}
LFS_POINTER = b"version https://git-lfs.github.com/spec/v1"
PLACEHOLDERS = {"...", "unknown", "todo", "tbd", "placeholder", "not_built", "latest", "stable", "n/a"}
MAX_JSON_BYTES = 8 * 1024 * 1024


class BundleError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise BundleError(message)


def fields(value, names, label):
    require(isinstance(value, dict), f"{label} must be an object")
    expected = set(names.split())
    missing = sorted(expected - value.keys())
    extra = sorted(value.keys() - expected)
    require(not missing and not extra, f"{label} has missing fields {missing} or unknown fields {extra}")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def concrete(value, label):
    if isinstance(value, str):
        require(
            bool(value.strip()) and value.strip().lower() not in PLACEHOLDERS and not (
                "<" in value and ">" in value
            ) and not any(0xD800 <= ord(char) <= 0xDFFF for char in value),
            f"{label} contains a placeholder, empty string or invalid Unicode",
        )
    elif isinstance(value, dict):
        for key, child in value.items():
            concrete(key, f"{label} key")
            concrete(child, f"{label}.{key}")
    elif isinstance(value, list):
        for i, child in enumerate(value):
            concrete(child, f"{label}[{i}]")
    else:
        require(type(value) in (int, bool), f"{label} must have a concrete JSON value")


def read_json(path):
    try:
        require(stat.S_ISREG(path.stat().st_mode), "JSON document must be a regular file")
        with path.open("rb") as handle:
            data = handle.read(MAX_JSON_BYTES + 1)
        require(len(data) <= MAX_JSON_BYTES, "JSON document exceeds the size limit")
        value = json.loads(data.decode("utf-8"), object_pairs_hook=unique_object)
        concrete(value, str(path))
        return value
    except (ValueError, RecursionError) as error:
        raise BundleError(f"{path}: {error}") from error


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def digest(value, label, *, prefixed=True, length=64):
    prefix = "sha256:" if prefixed else ""
    require(
        isinstance(value, str) and re.fullmatch(prefix + rf"[0-9a-f]{{{length}}}", value)
        and any(char != "0" for char in value[len(prefix):]),
        f"{label} must be a concrete {prefix} digest of {length} lowercase hex characters",
    )
    return value


def relative(value, label):
    require(
        isinstance(value, str)
        and all(
            part not in {".", "..", ".git"} and re.fullmatch(r"[A-Za-z0-9_.+@-]+", part)
            for part in value.split("/")
        ),
        f"{label} must be a canonical relative file path",
    )
    return value


def match(value, pattern, label):
    require(isinstance(value, str) and re.fullmatch(pattern, value), f"invalid {label}")
    return value


def names(value, pattern, label, *, required=False):
    require(isinstance(value, list) and (value or not required), f"{label} must be an array of names")
    for name in value:
        match(name, pattern, label)
    require(len(set(value)) == len(value), f"{label} contains duplicate names")
    return value


def file_digest(path):
    require(stat.S_ISREG(path.stat().st_mode), f"{path} must be a regular file")
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        first = handle.read(len(LFS_POINTER))
        require(first != LFS_POINTER, f"{path} is a Git LFS pointer")
        hasher.update(first)
        size = len(first)
        while chunk := handle.read(1024 * 1024):
            hasher.update(chunk)
            size += len(chunk)
    require(size > 0, f"{path} is empty")
    return hasher.hexdigest(), size


def validate_sources(sources):
    fields(sources, " ".join(REPOSITORIES), "sources")
    for repository, ref in sources.items():
        fields(ref, "commit tree", f"sources.{repository}")
        for key, value in ref.items():
            digest(value, f"sources.{repository}.{key}", prefixed=False, length=40)


def validate_flags(flags):
    require(isinstance(flags, dict), "flags must be an object")
    for name, wanted in {"BLK": 1, "NET": 1, "GPU": 0, "SKIP_LIBKRUNFW": 0}.items():
        value = flags.get(name, 0 if name == "SKIP_LIBKRUNFW" else None)
        require(type(value) is int and value == wanted, f"build requires {name}={wanted}")


def validate_pinned_files(pins, label, *, patches=False):
    require(isinstance(pins, list), f"{label} must be an array")
    paths = []
    ids = []
    for entry in pins:
        fields(entry, "repository path sha256" + (" id" if patches else ""), label)
        require(entry["repository"] in REPOSITORIES, f"unknown repository in {label}")
        relative(entry["path"], f"{label}.path")
        digest(entry["sha256"], f"{label}.sha256")
        paths.append((entry["repository"], entry["path"]))
        if patches:
            ids.append(match(entry["id"], r"[a-zA-Z0-9][a-zA-Z0-9._-]*", f"{label}.id"))
    require(len(set(paths)) == len(paths) and len(set(ids)) == len(ids), f"duplicate entries in {label}")
    return set(paths)


def validate_build_lock(lock):
    fields(
        lock,
        "schema sources gitlinks kernel toolchain_lock_digest flags patches required_libkrun_exports maximum_glibc",
        "build lock",
    )
    require(lock["schema"] == "RuntimeBuildLock/1", "unsupported build lock schema")
    validate_sources(lock["sources"])
    fields(lock["gitlinks"], "libkrun libkrunfw", "gitlinks")
    for repository, path in lock["gitlinks"].items():
        relative(path, f"gitlinks.{repository}")
    require(len(set(lock["gitlinks"].values())) == 2, "gitlink paths must be distinct")
    fields(lock["kernel"], "release archive_sha256 required_config", "kernel")
    match(lock["kernel"]["release"], r"[0-9]+\.[0-9]+\.[0-9]+(?:[-+.][A-Za-z0-9]+)*", "kernel.release")
    digest(lock["kernel"]["archive_sha256"], "kernel.archive_sha256")
    required_config = lock["kernel"]["required_config"]
    require(isinstance(required_config, dict) and required_config, "required_config must be a nonempty object")
    for key, value in required_config.items():
        match(key, r"CONFIG_[A-Z0-9_]+", "kernel config key")
        require(isinstance(value, str), f"required value for {key} must be a string")
    digest(lock["toolchain_lock_digest"], "toolchain_lock_digest")
    validate_flags(lock["flags"])
    validate_pinned_files(lock["patches"], "patches", patches=True)
    exports = names(lock["required_libkrun_exports"], r"krun_[A-Za-z0-9_]+", "required_libkrun_exports", required=True)
    require(DEFAULT_EXPORTS <= set(exports), "required_libkrun_exports omits default-path exports")
    match(lock["maximum_glibc"], r"[0-9]+\.[0-9]+(?:\.[0-9]+)?", "maximum_glibc")


def validate_toolchain_lock(lock):
    fields(lock, "schema builder_image versions lockfiles", "toolchain lock")
    require(lock["schema"] == "RuntimeToolchainLock/1", "unsupported toolchain lock schema")
    reference = match(lock["builder_image"], r"[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}", "builder_image")
    image, checksum = reference.split("@")
    require(not image.endswith((":latest", ":stable")), "builder_image must use an immutable reference")
    digest(checksum, "builder_image digest")
    fields(lock["versions"], "rust go node python cc ld", "toolchain versions")
    for tool, version in lock["versions"].items():
        match(version, r"[0-9]+\.[0-9]+(?:\.[0-9]+)*(?:[-+][A-Za-z0-9.]+)?", f"{tool} version")
    paths = validate_pinned_files(lock["lockfiles"], "lockfiles")
    require(
        {("smolvm", "Cargo.lock"), ("libkrun", "Cargo.lock")} <= paths,
        "toolchain lock must pin smolvm and libkrun Cargo.lock",
    )


def read_locks(build_path, toolchain_path):
    build, toolchain = read_json(build_path), read_json(toolchain_path)
    validate_build_lock(build)
    validate_toolchain_lock(toolchain)
    require(
        "sha256:" + file_digest(toolchain_path)[0] == build["toolchain_lock_digest"],
        "toolchain lock digest mismatch",
    )
    return build, toolchain


def git(root, *args):
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env.update(GIT_OPTIONAL_LOCKS="0", GIT_NO_REPLACE_OBJECTS="1", LC_ALL="C")
    result = subprocess.run(
        ["git", "-C", str(root), "-c", "core.fsmonitor=false", *args],
        capture_output=True,
        env=env,
    )
    require(result.returncode == 0, f"{root}: git {args[0]} failed: {os.fsdecode(result.stderr).strip()}")
    return result.stdout


def version(command, pattern, cwd):
    result = subprocess.run(command, capture_output=True, cwd=cwd, env={**os.environ, "LC_ALL": "C"}, text=True)
    require(result.returncode == 0, f"{command[0]} version check failed: {result.stderr.strip()}")
    version = re.search(pattern, result.stdout.splitlines()[0] if result.stdout else "")
    require(version is not None, f"could not read the installed {command[0]} version")
    return version[1]


def check_toolchain(lock, builder_image, checkout):
    require(builder_image == lock["builder_image"], "builder image does not match the toolchain lock")
    commands = {
        "rust": (["rustc", "--version"], r"^rustc ([^ ]+)"),
        "go": (["go", "version"], r"^go version go([^ ]+)"),
        "node": (["node", "--version"], r"^v([^ ]+)$"),
        "python": (["python3", "--version"], r"^Python ([^ ]+)$"),
        "cc": (["cc", "-dumpfullversion", "-dumpversion"], r"^([^ ]+)$"),
        "ld": (["ld", "--version"], r" ([^ ]+)$"),
    }
    for name, expected in lock["versions"].items():
        actual = version(*commands[name], checkout)
        require(actual == expected, f"{name} version mismatch: expected {expected}, got {actual}")


def check_checkout(root, reference, links):
    toplevel = Path(os.fsdecode(git(root, "rev-parse", "--show-toplevel").rstrip(b"\n"))).resolve()
    require(toplevel == root, f"{root} is not an initialized repository root")
    actual = git(root, "rev-parse", "HEAD", "HEAD^{tree}").decode("ascii").splitlines()
    require(actual == [reference["commit"], reference["tree"]], f"{root}: commit or tree mismatch")
    actual_links = {}
    for record in git(root, "ls-tree", "-rz", "HEAD").split(b"\0"):
        if not record:
            continue
        descriptor, path = record.split(b"\t", 1)
        mode, _, object_id = descriptor.split()
        if mode == b"160000":
            actual_links[os.fsdecode(path)] = object_id.decode("ascii")
        elif mode in (b"100644", b"100755"):
            checked = root / os.fsdecode(path)
            require(stat.S_ISREG(checked.lstat().st_mode), f"not a regular source file: {checked}")
            with checked.open("rb") as handle:
                require(handle.read(len(LFS_POINTER)) != LFS_POINTER, f"{checked} is a Git LFS pointer")
    require(actual_links == links, f"{root}: gitlinks do not match the build lock")

    status = git(
        root, "-c", "core.filemode=true", "status", "--porcelain=v1", "-z",
        "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none",
    )
    require(not status, f"{root} has local changes or build residue: {os.fsdecode(status.split(bytes(1))[0])!r}")
    # Git intentionally omits these index entries from normal status checks.
    for record in git(root, "ls-files", "-v", "-z").split(b"\0"):
        require(not record or record.startswith(b"H "), f"{root} has an unchecked index entry: {os.fsdecode(record)!r}")


def check_inputs(root, archive, build, toolchain):
    require("sha256:" + file_digest(archive)[0] == build["kernel"]["archive_sha256"], "kernel archive digest mismatch")
    root = root.resolve(strict=True)
    repositories = {"smolvm": root}
    references = build["sources"]
    links = {path: references[name]["commit"] for name, path in build["gitlinks"].items()}
    check_checkout(root, references["smolvm"], links)
    for name, path in build["gitlinks"].items():
        checkout = root / path
        require(checkout.resolve(strict=True) == checkout, f"{checkout} resolves through a symlink")
        check_checkout(checkout, references[name], {})
        repositories[name] = checkout
    for pin in toolchain["lockfiles"] + build["patches"]:
        path = repositories[pin["repository"]] / pin["path"]
        require(path.resolve(strict=True) == path, f"{path} resolves through a symlink")
        require("sha256:" + file_digest(path)[0] == pin["sha256"], f"{path}: pinned file digest mismatch")
    loader = (root / "src/agent/krun.rs").read_text(encoding="utf-8")
    requested = set(re.findall(r"load_sym!\(\s*(krun_[a-z0-9_]+)\s*\)", loader))
    require(requested and requested <= set(build["required_libkrun_exports"]),
            "build lock omits required symbols from the smolvm loader")

    firmware = repositories["libkrunfw"]
    recipe = (firmware / "Makefile").read_text(encoding="utf-8")
    for name, value in {
        "KERNEL_VERSION": "linux-" + build["kernel"]["release"],
        "KERNEL_SHA256": build["kernel"]["archive_sha256"].split(":", 1)[1],
    }.items():
        values = re.findall(rf"^{name}\s*=\s*(\S+)\s*$", recipe, re.M)
        require(values == [value], f"build lock {name} does not match libkrunfw/Makefile")
    actual_series = sorted(path.relative_to(firmware).as_posix() for path in (firmware / "patches").rglob("0*.patch"))
    locked_series = [pin["path"] for pin in build["patches"] if pin["repository"] == "libkrunfw" and
                     pin["path"].startswith("patches/") and re.fullmatch(r"0.*\.patch", Path(pin["path"]).name)]
    require(actual_series == locked_series, "build lock does not cover the ordered libkrunfw kernel patch series")
