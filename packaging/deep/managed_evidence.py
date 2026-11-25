"""Verify the release fixtures that enable optional managed Bundle contracts."""
import hashlib
import tarfile

from bundle_inputs import digest, fields, read_json, require

EVIDENCE = "metadata/managed-acceptance.json"
SYSTEM_TEMPLATE = "guest/system-debian.tar.gz"
SYSTEM_DISK = "system-debian.ext4"
MANAGED = {"managed_block_launch", "managed_parked_fork", "managed_statepoint", "lifecycle_policy"}
SCENARIOS = {
    "oci-capture": ("One held cut restored two distinct KVM children", "grandchild continued its captured process",
                    "Retry kept the successful execution", "Rollback fenced the previous execution",
                    "A separate restore from the original checkpoint reproduced the RAM and two-disk boundary"),
    "lifecycle": ("expired v1 lease suspect", "Hibernate after guest reporting had stopped",
                  "Two offline wake demands shared one exact restore"),
    "system-fork": ("System fork closed the old source stream",),
    "system-captures": ("A durable System StatePoint restored", "System hibernate fenced the old VMM"),
    "system-relay": ("real Guest relay enforced measured peer identity",),
}


def log_name(case):
    return f"metadata/managed/{case}.log"


def verify_run(case, path):
    require(path.is_file() and path.stat().st_size <= 4 * 1024 * 1024, f"invalid acceptance log for {case}")
    output = path.read_text(encoding="utf-8")
    require(all(marker in output for marker in SCENARIOS[case]), f"acceptance log is missing {case} results")


def verify(root, profile, artifacts):
    advertised = set(profile["contracts"]) & MANAGED
    if not advertised and EVIDENCE not in artifacts:
        return
    # This profile is certified as a combination on both launch paths.
    require(advertised == MANAGED and "system-debian-v1" in profile["boot_profiles"],
            "incomplete managed runtime verification profile")
    require(EVIDENCE in artifacts and SYSTEM_TEMPLATE in artifacts, "missing managed release evidence or System disk")
    evidence = read_json(root / EVIDENCE)
    fields(evidence, "schema build_lock_sha256 build_record_sha256 platformd_sha256 system_disk_sha256 system_disk_bytes acceptance_revisions runs", "managed evidence")
    require(evidence["schema"] == "RuntimeManagedAcceptance/1", "unsupported managed acceptance schema")
    for key, name in (("build_lock_sha256", "metadata/build-lock.json"), ("build_record_sha256", "metadata/build-record.json")):
        require(evidence[key] == "sha256:" + artifacts[name]["sha256"], f"managed acceptance belongs to a different {name}")
    digest(evidence["platformd_sha256"], "acceptance platformd digest")
    digest(evidence["system_disk_sha256"], "System disk digest")
    fields(evidence["acceptance_revisions"], "smolvm platform", "acceptance revisions")
    for repo, revision in evidence["acceptance_revisions"].items():
        digest(revision, f"{repo} acceptance revision", prefixed=False, length=40)
    size = evidence["system_disk_bytes"]
    require(type(size) is int and 0 < size <= 64 * 1024**3, "invalid System disk size")
    with tarfile.open(root / SYSTEM_TEMPLATE, "r|gz") as archive:
        entry = archive.next()
        require(entry is not None and entry.isfile() and entry.name == SYSTEM_DISK and entry.size == size,
                "System template needs the exact accepted ext4 disk")
        checksum = hashlib.sha256()
        with archive.extractfile(entry) as stream:
            for block in iter(lambda: stream.read(1 << 20), b""):
                checksum.update(block)
        require("sha256:" + checksum.hexdigest() == evidence["system_disk_sha256"] and archive.next() is None,
                "System template differs from the accepted disk")
    fields(evidence["runs"], " ".join(SCENARIOS), "managed acceptance runs")
    for case, result in evidence["runs"].items():
        fields(result, "log_sha256 harness_sha256", "acceptance run")
        digest(result["harness_sha256"], "acceptance harness digest")
        name = log_name(case)
        require(name in artifacts and result["log_sha256"] == "sha256:" + artifacts[name]["sha256"],
                f"managed acceptance log differs for {case}")
        verify_run(case, root / name)
