"""Verify the release fixtures that enable optional managed Bundle contracts."""
import hashlib
import ipaddress
import json
import tarfile

from bundle_inputs import BundleError, canonical_json, digest, fields, read_json, require

EVIDENCE = "metadata/managed-acceptance.json"
SYSTEM_TEMPLATE = "guest/system-debian.tar.gz"
SYSTEM_DISK = "system-debian.ext4"
MANAGED = {"managed_block_launch", "managed_parked_fork", "managed_statepoint", "lifecycle_policy"}
PRIVATE_NETWORK = "managed_private_network"
LIVE_RESOURCES = "managed_live_resources"
AUTHORIZED_SCOPE = "managed_authorized_scope"
AUTHORIZED_LIFECYCLE = "authorized_lifecycle"
LIFECYCLE_CASE = "system-lifecycle"
LIFECYCLE_SCHEMA = "RuntimeAuthorizedLifecycle/1"
AUTHORIZED_OWNER = "authorized_owner"
OWNER_CASES = ("system-tasks-delete", "system-rollback")
OWNER_SCHEMA = "RuntimeAuthorizedOwner/1"
SCHEMA_V1 = "RuntimeManagedAcceptance/1"
SCHEMA_V2 = "RuntimeManagedAcceptance/2"
SCHEMA_V3 = "RuntimeManagedAcceptance/3"
SCHEMA_V4 = "RuntimeManagedAcceptance/4"
AUTHORIZED_CASES = {
    "system-boot": ("system-boot", None),
    "system-fork": ("system-fork", None),
    "system-restore": ("system-restore", None),
    "network-native": ("private-tcp", "native"),
    "network-fixed": ("private-tcp", "fixed_internal"),
}
MEASUREMENT_PROTOCOLS = {
    "worker_resource", "capture_control", "statepoint",
    "guest_identity", "network_attachment", "block_attachment",
}
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


def verify_common_inputs(root, artifacts, evidence):
    for key, name in (("build_lock_sha256", "metadata/build-lock.json"), ("build_record_sha256", "metadata/build-record.json")):
        require(evidence[key] == "sha256:" + artifacts[name]["sha256"], f"managed acceptance belongs to a different {name}")
    for key in ("platformd_sha256", "system_disk_sha256"):
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


def authorized_contracts(evidence):
    require(isinstance(evidence, dict), "authorized evidence must be an object")
    runs = evidence.get("runs")
    require(isinstance(runs, dict) and "system-boot" in runs and runs.keys() <= AUTHORIZED_CASES.keys(),
            "authorized qualification needs a System boot and recognized completed cases")
    return {AUTHORIZED_SCOPE}


def authorized_owner_runs(evidence):
    if AUTHORIZED_OWNER not in evidence:
        return {}
    owner = evidence[AUTHORIZED_OWNER]
    fields(owner, "schema runs", "authorized owner operations")
    runs = owner["runs"]
    require(owner["schema"] == OWNER_SCHEMA and isinstance(runs, dict) and runs
            and runs.keys() <= set(OWNER_CASES), "unsupported authorized owner operation")
    return runs


def verify_authorized_log(case, path, measured):
    require(path.is_file() and 0 < path.stat().st_size <= 4 * 1024 * 1024, f"invalid authorized log for {case}")
    label, mode = (case, None) if case in (LIFECYCLE_CASE, *OWNER_CASES) else AUTHORIZED_CASES[case]
    prefix = "Authorized KVM "
    lines = path.read_text(encoding="utf-8").splitlines()
    require(bool(lines) and lines[-1].startswith(prefix), f"missing final authorized result for {case}")
    try:
        witnesses = [json.loads(line.removeprefix(prefix)) for line in lines if line.startswith(prefix)]
    except json.JSONDecodeError as error:
        raise BundleError(f"invalid authorized log for {case}") from error
    require(all(isinstance(value, dict) for value in witnesses), "invalid authorized witness")
    matches = [value for value in witnesses if value.get("case") == label and value.get("mode") == mode]
    require(len(matches) == 1 and matches[0].get("bundle_digest") == measured,
            f"missing singular authorized witness for {case} on its measurement Bundle")
    value = matches[0]

    def execution(key, ref):
        fields(ref, "instance_id incarnation_id execution_epoch", key)
        for identifier in ("instance_id", "incarnation_id"):
            require(isinstance(ref[identifier], str) and ref[identifier], f"missing {key} identity")
        require(type(ref["execution_epoch"]) is int and ref["execution_epoch"] > 0, f"invalid {key} epoch")
        return ref

    def ref(key):
        return execution(key, value.get(key))

    def marked(*keys):
        return all(isinstance(value.get(key), str) and value[key] for key in keys)

    def same_slot(old, new):
        return (old["instance_id"] == new["instance_id"]
                and old["incarnation_id"] != new["incarnation_id"]
                and new["execution_epoch"] == old["execution_epoch"] + 1)

    if case == "system-boot":
        require(ref("execution")["execution_epoch"] == 1 and marked("marker"), "missing initial System execution")
    elif case == "system-fork":
        source = ref("source")
        children = value.get("children")
        require(isinstance(children, list) and len(children) == 2 and marked("marker"), "missing independent children")
        executions = [source, *(execution("child", child) for child in children)]
        require(all(len({row[key] for row in executions}) == 3 for key in ("incarnation_id", "instance_id")),
                "System children share an execution")
    elif case == "system-restore":
        old, restored, saved = ref("original"), ref("restored"), ref("saved_execution")
        require(same_slot(old, saved)
                and all(restored[key] not in {old[key], saved[key]} for key in ("instance_id", "incarnation_id"))
                and marked("capture_id", "snapshot_id", "marker"), "missing System checkpoint and exact Hibernate resume")
    elif case == LIFECYCLE_CASE:
        original = ref("execution")
        protected, cleared = value.get("protected_revision"), value.get("cleared_revision")
        require(original == ref("standby") == ref("resumed")
                and type(protected) is int and protected > 0 and type(cleared) is int and cleared == protected + 1
                and marked("wake_operation_id", "released_lease_id"), "missing policy, held Standby and authorized resume")
        require(same_slot(original, ref("cold_started")) and marked("stop_operation_id", "start_operation_id")
                and value["stop_operation_id"] != value["start_operation_id"], "missing verified cold restart")
    elif case == "system-tasks-delete":
        ids = ("task_operation_id", "cancel_operation_id", "stop_operation_id", "delete_operation_id")
        require(same_slot(ref("original"), ref("execution"))
                and marked("task_id", "capture_id", "marker", *ids)
                and len({value[key] for key in ids}) == len(ids), "missing completed task and machine retirement")
    elif case == "system-rollback":
        require(same_slot(ref("original"), ref("restored"))
                and marked("snapshot_id", "operation_id", "rescue_capture_id", "marker"), "missing same-machine rollback")
    else:
        original, restored = ref("original"), ref("restored")
        receipts = value.get("business_receipts")
        opposite = "fixed_internal" if mode == "native" else "native"
        try:
            address = ipaddress.IPv4Address(value.get("private_ipv4", ""))
        except ipaddress.AddressValueError as error:
            raise BundleError("missing reserved private IPv4 address") from error
        require(same_slot(original, restored) and value.get("source_mode") == opposite
                and marked("capture_id", "operation_id", "attachment_id", "private_ipv4")
                and address.is_private and not (address.is_unspecified or address.is_loopback or address.is_link_local)
                and isinstance(receipts, dict) and len(receipts) == 2
                and all(request and type(count) is int and count == 1 for request, count in receipts.items()),
                "authorized private flow lacks a single exact wake and two unique business receipts")


def verify_authorized(root, profile, artifacts, evidence):
    base = ("schema build_lock_sha256 build_record_sha256 platformd_sha256 system_disk_sha256 system_disk_bytes "
            "system_template_sha256 acceptance_revisions measurement_bundle_digest runs")
    optional = [group for group in (AUTHORIZED_LIFECYCLE, AUTHORIZED_OWNER) if group in evidence]
    fields(evidence, " ".join((base, *optional)), "authorized evidence")
    advertised = set(profile["contracts"]) - MEASUREMENT_PROTOCOLS
    require(advertised == authorized_contracts(evidence) and "system-debian-v1" in profile["boot_profiles"],
            "authorized evidence differs from the advertised contract scope")
    with tarfile.open(root / "sources/smolvm.tar.gz", "r:gz") as archive:
        try:
            reader = archive.extractfile("crates/smolvm-runtime-protocol/src/measured_release.rs")
        except KeyError as error:
            raise BundleError("missing authorized release reader source") from error
        require(reader is not None, "missing authorized release reader source")
        with reader:
            code = reader.read(512 * 1024)
        require(SCHEMA_V4.encode() in code and AUTHORIZED_SCOPE.encode() in code,
                "locked Serve reader cannot advertise authorized release cases")
    verify_common_inputs(root, artifacts, evidence)
    require(evidence["system_template_sha256"] == "sha256:" + artifacts[SYSTEM_TEMPLATE]["sha256"],
            "authorized evidence uses a different System template")
    full_measurement = {name: 1 for name in MEASUREMENT_PROTOCOLS | MANAGED}
    measured = measurement_manifest(artifacts, evidence["build_record_sha256"], full_measurement)["bundle_digest"]
    require(evidence["measurement_bundle_digest"] == measured, "authorized measurement uses different binaries")
    runs = evidence["runs"]
    fields(runs["system-boot"], "log_sha256 harness_sha256", "authorized System boot run")
    harness = runs["system-boot"]["harness_sha256"]
    if AUTHORIZED_LIFECYCLE in evidence:
        lifecycle = evidence[AUTHORIZED_LIFECYCLE]
        fields(lifecycle, "schema log_sha256 harness_sha256", "authorized lifecycle run")
        require(lifecycle["schema"] == LIFECYCLE_SCHEMA, "unsupported authorized lifecycle schema")
        runs = {**runs, LIFECYCLE_CASE: {key: lifecycle[key] for key in ("log_sha256", "harness_sha256")}}
    selected = authorized_owner_runs(evidence)
    require("system-tasks-delete" not in selected or LIFECYCLE_CASE in runs, "task and deletion need measured lifecycle")
    require("system-rollback" not in selected or "system-restore" in runs, "rollback needs measured StatePoint restore")
    runs = {**runs, **selected}
    for case, result in runs.items():
        fields(result, "log_sha256 harness_sha256", "authorized acceptance run")
        digest(result["harness_sha256"], "authorized harness digest")
        require(result["harness_sha256"] == harness, "authorized cases use different fixture versions")
        name = log_name(case)
        require(name in artifacts and result["log_sha256"] == "sha256:" + artifacts[name]["sha256"],
                f"authorized acceptance log differs for {case}")
        verify_authorized_log(case, root / name, measured)


def verify(root, profile, artifacts):
    contracts = profile["contracts"]
    advertised = set(contracts) & MANAGED
    network = PRIVATE_NETWORK in contracts
    resources = LIVE_RESOURCES in contracts
    scoped = AUTHORIZED_SCOPE in contracts
    if not advertised and not network and not resources and not scoped and EVIDENCE not in artifacts:
        return
    require(EVIDENCE in artifacts and SYSTEM_TEMPLATE in artifacts, "missing managed release evidence or System disk")
    evidence = read_json(root / EVIDENCE)
    require(isinstance(evidence, dict), "managed evidence must be an object")
    if evidence.get("schema") == SCHEMA_V4:
        verify_authorized(root, profile, artifacts, evidence)
        return
    # Historical profiles are verified as a combination on both launch paths.
    require(not scoped and advertised == MANAGED and "system-debian-v1" in profile["boot_profiles"],
            "incomplete managed runtime verification profile")
    require(uses_private_network(evidence) == network, "private network contract and acceptance schema differ")
    require((evidence["schema"] == SCHEMA_V3) == resources, "live resource contract and acceptance schema differ")
    base_fields = "schema build_lock_sha256 build_record_sha256 platformd_sha256 system_disk_sha256 system_disk_bytes acceptance_revisions counter_sha256 vsock_probe_sha256 runs"
    fields(evidence, base_fields + (" private_network" if network else "") + (" live_resources" if resources else ""), "managed evidence")
    verify_common_inputs(root, artifacts, evidence)
    for key in ("counter_sha256", "vsock_probe_sha256"):
        digest(evidence[key], key)
    fields(evidence["runs"], " ".join(SCENARIOS), "managed acceptance runs")
    for case, result in evidence["runs"].items():
        fields(result, "log_sha256 harness_sha256", "acceptance run")
        verify_log(root, artifacts, case, result)
    if network:
        verify_private_network(root, profile, artifacts, evidence)
    if resources:
        verify_live_resources(root, profile, artifacts, evidence)
