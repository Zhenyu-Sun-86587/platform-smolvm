import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from bundle import BUILD_LOCK, BUILD_RECORD, PROTOCOLS, artifact_index, list_files
from bundle_inputs import BundleError
import managed_evidence as managed


def checksum(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


class ManagedEvidenceTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        for name in set(managed.MEASURED_ARTIFACTS.values()) | {BUILD_LOCK, BUILD_RECORD}:
            self.put(name, name.encode())
        disk = b"fixture disk bytes"
        template = self.root / managed.SYSTEM_TEMPLATE
        with tarfile.open(template, "w:gz") as out:
            member = tarfile.TarInfo(managed.SYSTEM_DISK)
            member.size = len(disk)
            out.addfile(member, io.BytesIO(disk))
        self.evidence = {"schema": managed.SCHEMA_V1, "build_lock_sha256": checksum(BUILD_LOCK.encode()),
                         "build_record_sha256": checksum(BUILD_RECORD.encode()), "platformd_sha256": checksum(b"platformd"),
                         "system_disk_sha256": checksum(disk), "system_disk_bytes": len(disk),
                         "acceptance_revisions": {"smolvm": "a" * 40, "platform": "b" * 40},
                         "counter_sha256": checksum(b"counter"), "vsock_probe_sha256": checksum(b"probe"),
                         "runs": {name: self.log(name, markers) for name, markers in managed.SCENARIOS.items()}}
        self.profile = {"contracts": dict.fromkeys((*PROTOCOLS, *managed.MANAGED), 1),
                        "boot_profiles": ["oci-v1", "system-debian-v1"]}

    def put(self, name, data):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)

    def log(self, case, lines):
        data = ("\n".join(lines) + "\n").encode()
        self.put(managed.log_name(case), data)
        return {"harness_sha256": checksum(b"system fixture"), "log_sha256": checksum(data)}

    def inventory(self):
        return artifact_index(self.root, list_files(self.root))

    def add_network(self):
        measured = managed.measurement_manifest(self.inventory(), self.evidence["build_record_sha256"],
                                                self.profile["contracts"])["bundle_digest"]
        runs = {}
        for case, (source, target) in managed.PACKET_DIRECTIONS.items():
            label = f"Private packet round trip {source} -> {target} on {measured}."
            runs[case] = {**self.log(case, (*managed.PACKET_MARKERS, label)), "source_mode": source, "target_mode": target}
        self.evidence.update(schema=managed.SCHEMA_V2, private_network={"runs": runs, "measurement_bundle_digest": measured,
            "system_template_sha256": checksum((self.root / managed.SYSTEM_TEMPLATE).read_bytes())})
        self.profile["contracts"][managed.PRIVATE_NETWORK] = 1

    def verify(self):
        self.put(managed.EVIDENCE, json.dumps(self.evidence).encode())
        managed.verify(self.root, self.profile, self.inventory())

    def test_v1_still_verifies(self):
        self.verify()
        self.profile["contracts"][managed.PRIVATE_NETWORK] = 1
        with self.assertRaisesRegex(BundleError, "contract and acceptance schema differ"):
            self.verify()

    def test_v2_binds_distinct_directions_and_runtime(self):
        self.add_network()
        self.verify()
        runs = self.evidence["private_network"]["runs"]
        original = runs["packet-fixed"].copy()
        data = (self.root / managed.log_name("packet-native")).read_bytes()
        self.put(managed.log_name("packet-fixed"), data)
        runs["packet-fixed"]["log_sha256"] = checksum(data)
        with self.assertRaisesRegex(BundleError, "wrong measured direction or Bundle"):
            self.verify()
        runs["packet-fixed"] = original
        self.put("smolvm-resourced", b"another Worker executable")
        with self.assertRaisesRegex(BundleError, "different runtime artifacts"):
            self.verify()

    def test_v3_binds_parked_and_growth_evidence_to_the_same_release(self):
        measured = managed.measurement_manifest(self.inventory(), self.evidence["build_record_sha256"],
                                                self.profile["contracts"])["bundle_digest"]
        runs = {}
        for case, profile in managed.RESOURCE_PROFILES.items():
            label = f"Resource measurement {profile} on {measured}."
            runs[case] = {**self.log(case, (*managed.RESOURCE_MARKERS[case], label)), "profile": profile}
        self.evidence.update(schema=managed.SCHEMA_V3, live_resources={
            "runs": runs, "measurement_bundle_digest": measured,
            "system_template_sha256": checksum((self.root / managed.SYSTEM_TEMPLATE).read_bytes())})
        self.profile["contracts"][managed.LIVE_RESOURCES] = 1
        self.verify()
        self.add_network()
        self.evidence["schema"] = managed.SCHEMA_V3
        self.verify()
        runs["live-resources-hot"]["profile"] = "parked-capture"
        with self.assertRaisesRegex(BundleError, "wrong execution profile"):
            self.verify()


if __name__ == "__main__":
    unittest.main()
