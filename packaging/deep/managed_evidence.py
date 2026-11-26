"""Verify the release fixtures that enable optional managed Bundle contracts."""
import hashlib
import tarfile

from bundle_inputs import canonical_json, digest, fields, read_json, require

EVIDENCE = "metadata/managed-acceptance.json"
SYSTEM_TEMPLATE = "guest/system-debian.tar.gz"
SYSTEM_DISK = "system-debian.ext4"
MANAGED = {"managed_block_launch", "managed_parked_fork", "managed_statepoint", "lifecycle_policy"}
PRIVATE_NETWORK = "managed_private_network"
LIVE_RESOURCES = "managed_live_resources"
SCHEMA_V1 = "RuntimeManagedAcceptance/1"
SCHEMA_V2 = "RuntimeManagedAcceptance/2"
SCHEMA_V3 = "RuntimeManagedAcceptance/3"
PACKET_DIRECTIONS = {
    "packet-native": ("fixed_internal", "native"),
    "packet-fixed": ("native", "fixed_internal"),
}
# Measure the runtime files actually loaded. The two library aliases in the
# fixture point at these same bytes in the installed archive.
MEASURED_ARTIFACTS = {
    "smolvm-bin": "smolvm-bin",
    "smolvm-resourced": "smolvm-resourced",
    "lib/libkrun.so.2": "lib/libkrun.so",
    "lib/libkrunfw.so.5": "lib/libkrunfw.so",
    "guest/oci-rootfs.tar.gz": "guest/oci-rootfs.tar.gz",
    "guest/identity-helper": "guest/identity-helper",
    "guest/agent": "guest/agent",
}
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
PACKET_MARKERS = (
    "Private TCP woke a real standby VMM once.",
    "A private SYN restored a durable VM at its reserved address and a new execution epoch.",
    "One authorized UDP datagram crossed durable wake. Oversized data woke the VM and was dropped.",
)
RESOURCE_PROFILES = {"live-resources": "parked-capture", "live-resources-hot": "live-compute"}
RESOURCE_MARKERS = {
    "live-resources": ("After platformd was killed during balloon adjustment",
                       "Public parked-capture settings matched the live Linux CPU set"),
    "live-resources-hot": ("Public live-compute settings matched the live Linux CPU set",
                           "Verified 1500 millicpus and restored the two-slot kernel allocation."),
}


def log_name(case):
    return f"metadata/managed/{case}.log"


def measurement_manifest(artifacts, build_record, contracts):
    manifest = {
        "schema": "RuntimeBundle/1", "platform": "linux-x86_64",
        "guest": {"boot_profiles": ["oci-v1", "system-debian-v1"]},
        "artifacts": [{**artifacts[installed], "path": measured}
                      for measured, installed in MEASURED_ARTIFACTS.items()],
        "contracts": {**{key: value for key, value in contracts.items() if key not in (PRIVATE_NETWORK, LIVE_RESOURCES)},
                      "state_reader_id": "managed-acceptance:" + build_record},
    }
    manifest["bundle_digest"] = "sha256:" + hashlib.sha256(canonical_json(manifest)).hexdigest()
    return manifest


def uses_private_network(evidence):
    require(isinstance(evidence, dict) and evidence.get("schema") in (SCHEMA_V1, SCHEMA_V2, SCHEMA_V3),
            "unsupported managed acceptance schema")
    return evidence["schema"] == SCHEMA_V2 or (evidence["schema"] == SCHEMA_V3 and "private_network" in evidence)


def verify_run(case, path, measured_bundle=None):
    require(path.is_file() and path.stat().st_size <= 4 * 1024 * 1024, f"invalid acceptance log for {case}")
    output = path.read_text(encoding="utf-8")
    if case in PACKET_DIRECTIONS:
        source, target = PACKET_DIRECTIONS[case]
        label = f"Private packet round trip {source} -> {target} on {measured_bundle}."
        require(output.splitlines()[-1:] == [label], f"wrong measured direction or Bundle for {case}")
        markers = PACKET_MARKERS
    elif case in RESOURCE_PROFILES:
        label = f"Resource measurement {RESOURCE_PROFILES[case]} on {measured_bundle}."
        require(output.splitlines()[-1:] == [label], f"wrong measured resource profile or Bundle for {case}")
        markers = RESOURCE_MARKERS[case]
    else:
        markers = SCENARIOS[case]
    require(all(marker in output for marker in markers), f"acceptance log is missing {case} results")


def verify_log(root, artifacts, case, result, measured_bundle=None):
    digest(result["harness_sha256"], "acceptance harness digest")
    name = log_name(case)
    require(name in artifacts and result["log_sha256"] == "sha256:" + artifacts[name]["sha256"],
            f"managed acceptance log differs for {case}")
    verify_run(case, root / name, measured_bundle)


def verify_measurement_inputs(group, profile, artifacts, evidence, label):
    fields(group, "measurement_bundle_digest system_template_sha256 runs", f"{label} acceptance")
    measured = measurement_manifest(artifacts, evidence["build_record_sha256"], profile["contracts"])["bundle_digest"]
    require(group["measurement_bundle_digest"] == measured, f"{label} was measured with different runtime artifacts")
    require(group["system_template_sha256"] == "sha256:" + artifacts[SYSTEM_TEMPLATE]["sha256"],
            f"{label} acceptance refers to a different System template")
    return measured


def verify_private_network(root, profile, artifacts, evidence):
    network = evidence["private_network"]
    measured = verify_measurement_inputs(network, profile, artifacts, evidence, "private network")
    fields(network["runs"], " ".join(PACKET_DIRECTIONS), "private network acceptance runs")
    for case, (source, target) in PACKET_DIRECTIONS.items():
        result = network["runs"][case]
        fields(result, "source_mode target_mode log_sha256 harness_sha256", "private network acceptance run")
        require((result["source_mode"], result["target_mode"]) == (source, target), f"wrong private network direction for {case}")
        require(result["harness_sha256"] == evidence["runs"]["system-fork"]["harness_sha256"],
                f"private network {case} has a different System harness")
        verify_log(root, artifacts, case, result, measured)


def verify_live_resources(root, profile, artifacts, evidence):
    resource = evidence["live_resources"]
    measured = verify_measurement_inputs(resource, profile, artifacts, evidence, "live resources")
    fields(resource["runs"], " ".join(RESOURCE_PROFILES), "live resource acceptance runs")
    for case, expected in RESOURCE_PROFILES.items():
        result = resource["runs"][case]
        fields(result, "profile log_sha256 harness_sha256", "live resource acceptance run")
        require(result["profile"] == expected, f"wrong execution profile for {case}")
        require(result["harness_sha256"] == evidence["runs"]["system-fork"]["harness_sha256"],
                f"{case} has a different System harness")
        verify_log(root, artifacts, case, result, measured)


def verify(root, profile, artifacts):
    advertised = set(profile["contracts"]) & MANAGED
    network = PRIVATE_NETWORK in profile["contracts"]
    resources = LIVE_RESOURCES in profile["contracts"]
    if not advertised and not network and not resources and EVIDENCE not in artifacts:
        return
    # This profile is certified as a combination on both launch paths.
    require(advertised == MANAGED and "system-debian-v1" in profile["boot_profiles"],
            "incomplete managed runtime verification profile")
    require(EVIDENCE in artifacts and SYSTEM_TEMPLATE in artifacts, "missing managed release evidence or System disk")
    evidence = read_json(root / EVIDENCE)
    require(uses_private_network(evidence) == network, "private network contract and acceptance schema differ")
    require((evidence["schema"] == SCHEMA_V3) == resources, "live resource contract and acceptance schema differ")
    base_fields = "schema build_lock_sha256 build_record_sha256 platformd_sha256 system_disk_sha256 system_disk_bytes acceptance_revisions counter_sha256 vsock_probe_sha256 runs"
    fields(evidence, base_fields + (" private_network" if network else "") + (" live_resources" if resources else ""), "managed evidence")
    for key, name in (("build_lock_sha256", "metadata/build-lock.json"), ("build_record_sha256", "metadata/build-record.json")):
        require(evidence[key] == "sha256:" + artifacts[name]["sha256"], f"managed acceptance belongs to a different {name}")
    for key in ("platformd_sha256", "system_disk_sha256", "counter_sha256", "vsock_probe_sha256"):
        digest(evidence[key], key)
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
        verify_log(root, artifacts, case, result)
    if network:
        verify_private_network(root, profile, artifacts, evidence)
    if resources:
        verify_live_resources(root, profile, artifacts, evidence)
