"""Platform RuntimeBundle/1 checks without installing a runtime or starting a VM."""

import hashlib
import io
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "packaging/deep"))
import bundle
import managed_evidence
from bundle_inputs import BundleError, DEFAULT_EXPORTS, check_checkout, check_inputs, git, read_locks


def sha(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def toolchain():
    return {
        "schema": "RuntimeToolchainLock/1",
        "builder_image": "registry.invalid/ci/builder@" + sha(b"test builder"),
        "versions": {"rust": "1.88.0", "go": "1.25.0", "node": "22.0.0", "python": "3.12.0", "cc": "13.3.0", "ld": "2.42"},
        "lockfiles": [
            {"repository": name, "path": "Cargo.lock", "sha256": sha(f"{name} lock\n".encode())}
            for name in ("smolvm", "libkrun")
        ],
    }


def build_lock(sources, toolchain_digest, archive_bytes=b"test archive"):
    return {
        "schema": "RuntimeBuildLock/1",
        "sources": sources,
        "gitlinks": {"libkrun": "libkrun", "libkrunfw": "libkrunfw"},
        "kernel": {
            "release": "6.18.60",
            "archive_sha256": sha(archive_bytes),
            "required_config": {"CONFIG_VSOCKETS": "y", "CONFIG_DRM": "n"},
        },
        "toolchain_lock_digest": toolchain_digest,
        "flags": {"BLK": 1, "NET": 1, "GPU": 0},
        "patches": [],
        "required_libkrun_exports": sorted(DEFAULT_EXPORTS),
        "maximum_glibc": "2.34",
    }


class BundleTests(unittest.TestCase):
    def setUp(self):
        work = tempfile.TemporaryDirectory()
        self.addCleanup(work.cleanup)
        self.root = Path(work.name)
        self.stage = self.root / "stage"
        self.stage.mkdir()
        self.output = self.root / "bundle"
        for name in bundle.REQUIRED_FILES:
            self.write(name, (name + "\n").encode())
        for name in bundle.EXECUTABLES:
            (self.stage / name).chmod(0o755)

        sources = {
            name: {"commit": hashlib.sha1(name.encode()).hexdigest(), "tree": hashlib.sha1((name + "-tree").encode()).hexdigest()}
            for name in ("smolvm", "libkrun", "libkrunfw")
        }
        self.seed = {"schema": "RuntimeRootfsSeed/1", "recipe": bundle.ROOTFS_RECIPE,
                     **{name: sha(name.encode()) for name in bundle.ROOTFS_INPUT_DIGESTS}}
        self.seed_path = "packaging/rootfs-seed.json"
        self.json(bundle.ROOTFS_INPUT_LOCK, self.seed)
        tools = toolchain()
        tools["lockfiles"].append({"repository": "smolvm", "path": self.seed_path,
                                   "sha256": sha((self.stage / bundle.ROOTFS_INPUT_LOCK).read_bytes())})
        self.json(bundle.TOOLCHAIN_LOCK, tools)
        lock = build_lock(sources, sha((self.stage / bundle.TOOLCHAIN_LOCK).read_bytes()))
        self.json(bundle.BUILD_LOCK, lock)
        self.write(bundle.CONFIG, b"CONFIG_VSOCKETS=y\n# CONFIG_DRM is not set\n")
        self.write(bundle.LIBKRUN, b"test libkrun embeds " + (self.stage / bundle.INIT).read_bytes())
        self.write(bundle.PROVENANCE, "".join(
            f"{name}={sources[name]['commit']}\n" for name in ("libkrun", "libkrunfw")
        ).encode())
        for label, name in bundle.PROFILES.items():
            self.json(name, {"profile": label})
        self.profile = {
            "schema": "RuntimeVerificationProfile/1",
            "boot_profiles": ["oci-v1"],
            "contracts": dict.fromkeys(bundle.PROTOCOLS, 1),
            "state_reader_id": "reader-1",
            "required_kvm_caps": ["KVM_CAP_USER_MEMORY"],
            "optional_features": [],
        }
        self.json(bundle.VERIFICATION_PROFILE, self.profile)
        self.make_rootfs((self.stage / bundle.AGENT).read_bytes())
        self.write_record()
        self.inspection = patch.object(bundle, "check_elf_artifacts")
        self.inspection.start()
        self.addCleanup(self.inspection.stop)

    def write(self, name, data):
        path = self.stage / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)

    def json(self, name, data):
        self.write(name, json.dumps(data).encode())

    def make_rootfs(self, agent, helper=None):
        with tarfile.open(self.stage / "guest/oci-rootfs.tar.gz", "w:gz") as archive:
            member = tarfile.TarInfo("./usr/local/bin/smolvm-agent")
            member.mode = 0o755
            member.size = len(agent)
            archive.addfile(member, io.BytesIO(agent))
            if helper is not None:
                member = tarfile.TarInfo("./sbin/smolvm-identity-helper")
                member.mode, member.size = 0o755, len(helper)
                archive.addfile(member, io.BytesIO(helper))

    def write_record(self, *, refresh_rootfs=True):
        if refresh_rootfs:
            self.json(bundle.ROOTFS_PROVENANCE, {
                "schema": "RuntimeRootfsProvenance/1",
                "input_lock_path": self.seed_path,
                "input_lock_sha256": sha((self.stage / bundle.ROOTFS_INPUT_LOCK).read_bytes()),
                "recipe_sha256": sha(b"source recipe"),
                "agent_sha256": sha((self.stage / bundle.AGENT).read_bytes()),
                "rootfs_sha256": sha((self.stage / bundle.ROOTFS).read_bytes()),
                **{name: self.seed[name] for name in bundle.ROOTFS_INPUT_DIGESTS},
            })
        self.json(bundle.BUILD_RECORD, {
            "schema": "RuntimeBuildRecord/1",
            "build_lock_digest": sha((self.stage / bundle.BUILD_LOCK).read_bytes()),
            "outputs": {
                name: sha((self.stage / name).read_bytes())
                for name in {*bundle.RUNTIME_OUTPUTS, bundle.IDENTITY_HELPER}
                if (self.stage / name).exists()
            },
        })

    def rewrite_manifest(self, change):
        path = self.output / bundle.MANIFEST
        manifest = json.loads(path.read_bytes())
        change(manifest)
        manifest["bundle_digest"] = bundle.manifest_digest(manifest)
        path.chmod(0o644)
        path.write_text(json.dumps(manifest, indent=2))

    def test_pack_verify_and_refuse_existing_output(self):
        expected = bundle.pack_bundle(self.stage, self.output)
        self.assertEqual(bundle.verify_bundle(self.output, expected), expected)
        self.assertEqual(bundle.pack_bundle(self.stage, self.root / "another"), expected)
        self.assertEqual((self.output / bundle.MANIFEST).stat().st_mode & 0o777, 0o444)
        with self.assertRaisesRegex(BundleError, "output already exists"):
            bundle.pack_bundle(self.stage, self.output)
        with self.assertRaisesRegex(BundleError, "unexpected bundle digest"):
            bundle.verify_bundle(self.output, sha(b"other bundle"))

    def test_signature_is_excluded_from_digest(self):
        expected = bundle.pack_bundle(self.stage, self.output)
        self.rewrite_manifest(lambda manifest: manifest.update(signature="signed-by-release-service"))
        self.assertEqual(bundle.verify_bundle(self.output), expected)

    def test_managed_contracts_require_bound_evidence_and_the_exact_system_disk(self):
        self.profile["boot_profiles"].append("system-debian-v1")
        self.profile["contracts"].update(dict.fromkeys(managed_evidence.MANAGED, 1))
        self.json(bundle.VERIFICATION_PROFILE, self.profile)
        with self.assertRaisesRegex(BundleError, "missing managed release evidence"):
            bundle.pack_bundle(self.stage, self.output)
        disk = b"release disk bytes"
        with tarfile.open(self.stage / managed_evidence.SYSTEM_TEMPLATE, "w:gz") as archive:
            member = tarfile.TarInfo(managed_evidence.SYSTEM_DISK)
            member.size = len(disk)
            archive.addfile(member, io.BytesIO(disk))
        evidence = {
            "schema": "RuntimeManagedAcceptance/1",
            "build_lock_sha256": sha((self.stage / bundle.BUILD_LOCK).read_bytes()),
            "build_record_sha256": sha((self.stage / bundle.BUILD_RECORD).read_bytes()),
            "platformd_sha256": sha(b"platformd"), "system_disk_sha256": sha(disk),
            "system_disk_bytes": len(disk), "acceptance_revisions": {"smolvm": "1"*40, "platform": "2"*40},
            "counter_sha256": sha(b"counter"), "vsock_probe_sha256": sha(b"probe"),
            "runs": {},
        }
        for case, markers in managed_evidence.SCENARIOS.items():
            log = "\n".join(markers).encode()
            self.write(managed_evidence.log_name(case), log)
            evidence["runs"][case] = {"log_sha256": sha(log), "harness_sha256": sha(case.encode())}
        self.json(managed_evidence.EVIDENCE, evidence)
        expected = bundle.pack_bundle(self.stage, self.output)
        self.assertEqual(bundle.verify_bundle(self.output), expected)
        for key, reason in (("system_disk_sha256", "differs from the accepted disk"),
                            ("build_record_sha256", "different metadata/build-record")):
            self.json(managed_evidence.EVIDENCE, {**evidence, key: sha(b"different")})
            with self.assertRaisesRegex(BundleError, reason):
                bundle.pack_bundle(self.stage, self.root / "bad")
        self.json(managed_evidence.EVIDENCE, evidence)
        self.write(managed_evidence.log_name("lifecycle"), b"unexpected")
        with self.assertRaisesRegex(BundleError, "acceptance log differs"):
            bundle.pack_bundle(self.stage, self.root / "bad")

    def test_install_is_read_only_and_preserves_an_existing_release(self):
        expected = bundle.pack_bundle(self.stage, self.output)
        installed = self.root / "installed"
        self.assertEqual(bundle.install_bundle(self.output, installed, expected), expected)
        self.assertEqual(bundle.verify_installed_bundle(installed, expected), expected)
        inode = installed.stat().st_ino
        self.assertEqual(bundle.install_bundle(self.output, installed, expected), expected)
        with self.assertRaisesRegex(bundle.BundleError, "unexpected bundle digest"):
            bundle.install_bundle(self.output, installed, sha(b"other bundle"))
        self.assertEqual(installed.stat().st_ino, inode)

        (installed / bundle.MANIFEST).chmod(0o644)
        with self.assertRaisesRegex(bundle.BundleError, "installed artifact is writable"):
            bundle.install_bundle(self.output, installed, expected)

    def test_reject_unindexed_files_and_modified_payload(self):
        bundle.pack_bundle(self.stage, self.output)
        stray = self.output / "extra"
        stray.write_text("uncatalogued")
        with self.assertRaisesRegex(BundleError, "unindexed bundle artifacts"):
            bundle.verify_bundle(self.output)
        stray.unlink()
        artifact = self.output / "NOTICE"
        artifact.chmod(0o644)
        artifact.write_bytes(b"x" * artifact.stat().st_size)
        with self.assertRaisesRegex(BundleError, "artifact digest or order mismatch"):
            bundle.verify_bundle(self.output)

    def test_reject_manifest_paths_and_recomputed_unbound_metadata(self):
        for change, message in (
            (lambda data: data["artifacts"][0].update(path="../LICENSE"), "canonical relative"),
            (lambda data: data["artifacts"].append(data["artifacts"][0]), "duplicate artifact path"),
            (lambda data: data["host_requirements"].update(required_kvm_caps=[]), "manifest fields do not match"),
        ):
            bundle.pack_bundle(self.stage, self.output)
            self.rewrite_manifest(change)
            with self.assertRaisesRegex(BundleError, message):
                bundle.verify_bundle(self.output)
            shutil.rmtree(self.output)

    def test_reject_stale_outputs_and_rootfs_agent(self):
        self.write(bundle.FIRMWARE, b"stale firmware")
        with self.assertRaisesRegex(BundleError, "build record output digest mismatch"):
            bundle.pack_bundle(self.stage, self.output)
        self.assertFalse(self.output.exists())
        self.make_rootfs(b"x" * (self.stage / bundle.AGENT).stat().st_size)
        self.write_record()
        with self.assertRaisesRegex(BundleError, "different smolvm-agent"):
            bundle.pack_bundle(self.stage, self.output)

    def test_reject_relabelled_source_lock(self):
        path = self.stage / bundle.BUILD_LOCK
        lock = json.loads(path.read_bytes())
        lock["sources"]["smolvm"]["commit"] = hashlib.sha1(b"new commit").hexdigest()
        self.json(bundle.BUILD_LOCK, lock)
        with self.assertRaisesRegex(BundleError, "different build lock"):
            bundle.pack_bundle(self.stage, self.output)

    def test_reject_links_lfs_and_unknown_contract(self):
        link = self.stage / "external"
        link.symlink_to(self.root / "missing")
        with self.assertRaisesRegex(BundleError, "regular files"):
            bundle.pack_bundle(self.stage, self.output)
        link.unlink()
        self.write("LICENSE", b"version https://git-lfs.github.com/spec/v1\n")
        with self.assertRaisesRegex(BundleError, "Git LFS pointer"):
            bundle.pack_bundle(self.stage, self.output)
        self.write("LICENSE", b"a distribution license")
        self.profile["contracts"]["statepoint"] = 2
        self.json(bundle.VERIFICATION_PROFILE, self.profile)
        with self.assertRaisesRegex(BundleError, "unsupported contract"):
            bundle.pack_bundle(self.stage, self.output)

    def test_library_stage_cannot_pass_full_pack(self):
        for name in bundle.REQUIRED_FILES - bundle.LIBRARY_FILES:
            (self.stage / name).unlink()
        self.json(bundle.BUILD_RECORD, {
            "schema": "RuntimeBuildRecord/1",
            "build_lock_digest": sha((self.stage / bundle.BUILD_LOCK).read_bytes()),
            "outputs": {name: sha((self.stage / name).read_bytes()) for name in bundle.LIBRARY_OUTPUTS},
        })
        bundle.check_libraries(self.stage)
        with self.assertRaisesRegex(BundleError, "missing required bundle artifacts"):
            bundle.pack_bundle(self.stage, self.output)

    def test_runtime_stage_checks_seed_lock_and_refuses_missing_helper_at_pack(self):
        (self.stage / bundle.IDENTITY_HELPER).unlink()
        self.write_record()
        bundle.check_runtime(self.stage)
        with self.assertRaisesRegex(BundleError, "missing required bundle artifacts: guest/identity-helper"):
            bundle.pack_bundle(self.stage, self.output)
        provenance = json.loads((self.stage / bundle.ROOTFS_PROVENANCE).read_bytes())
        provenance["seed_sha256"] = sha(b"a different seed")
        self.json(bundle.ROOTFS_PROVENANCE, provenance)
        self.write_record(refresh_rootfs=False)
        with self.assertRaisesRegex(BundleError, "seed_sha256 differs from the input lock"):
            bundle.check_runtime(self.stage)

    def test_legacy_rootfs_requires_explicit_compatibility_and_cannot_pack(self):
        for path in bundle.ROOTFS_METADATA:
            (self.stage / path).unlink()
        self.write_record(refresh_rootfs=False)
        with self.assertRaisesRegex(BundleError, "missing required bundle artifacts: metadata/rootfs"):
            bundle.pack_bundle(self.stage, self.output)
        manifest = bundle.build_manifest(self.stage, bundle.list_files(self.stage), allow_legacy_rootfs=True)
        self.json(bundle.MANIFEST, manifest)
        with self.assertRaisesRegex(BundleError, "missing required bundle artifacts: metadata/rootfs"):
            bundle.verify_bundle(self.stage)
        self.assertEqual(bundle.verify_bundle(self.stage, allow_legacy_rootfs=True), manifest["bundle_digest"])

    def test_measured_helper_must_match_the_oci_bytes(self):
        toolchain = json.loads((self.stage / bundle.TOOLCHAIN_LOCK).read_bytes())
        toolchain["lockfiles"].append({"repository": "smolvm", "path": "crates/smolvm-identity-helper/Cargo.toml",
                                      "sha256": sha(b"helper crate")})
        self.json(bundle.TOOLCHAIN_LOCK, toolchain)
        build = json.loads((self.stage / bundle.BUILD_LOCK).read_bytes())
        build["toolchain_lock_digest"] = sha((self.stage / bundle.TOOLCHAIN_LOCK).read_bytes())
        self.json(bundle.BUILD_LOCK, build)
        helper = (self.stage / bundle.IDENTITY_HELPER).read_bytes()
        for actual in (helper, b"x" * len(helper)):
            self.make_rootfs((self.stage / bundle.AGENT).read_bytes(), actual)
            self.write_record()
            provenance = json.loads((self.stage / bundle.ROOTFS_PROVENANCE).read_bytes())
            provenance.update(schema="RuntimeRootfsProvenance/2", identity_helper_sha256=sha(helper))
            self.json(bundle.ROOTFS_PROVENANCE, provenance)
            self.write_record(refresh_rootfs=False)
            if actual == helper:
                bundle.check_runtime(self.stage)
            else:
                with self.assertRaisesRegex(BundleError, "different smolvm-identity-helper"):
                    bundle.check_runtime(self.stage)

    @unittest.skipUnless(all(shutil.which(tool) for tool in ("as", "ld", "readelf")), "requires GNU binutils")
    def test_pack_with_real_static_elf_and_soname_files(self):
        self.inspection.stop()
        source = self.root / "fixture.S"
        obj = self.root / "fixture.o"
        source.write_text(".text\n.global _start\n_start:\n  ret\n")
        subprocess.run(["as", "--64", "-o", obj, source], check=True)
        subprocess.run(["ld", "-m", "elf_x86_64", "-o", self.stage / bundle.INIT, obj], check=True)
        for name in (bundle.AGENT, bundle.IDENTITY_HELPER, "smolvm-bin", "smolvm-resourced"):
            self.write(name, (self.stage / bundle.INIT).read_bytes())
        self.make_rootfs((self.stage / bundle.AGENT).read_bytes())

        source.write_text(
            '.section .rodata\n.incbin "' + str(self.stage / bundle.INIT) + '"\n.text\n'
            + "".join(f".global {name}\n.type {name}, @function\n{name}:\n  ret\n" for name in sorted(DEFAULT_EXPORTS))
        )
        subprocess.run(["as", "--64", "-o", obj, source], check=True)
        for name, soname in ((bundle.LIBKRUN, "libkrun.so.2"), (bundle.FIRMWARE, "libkrunfw.so.5")):
            subprocess.run(["ld", "-shared", "-soname", soname, "-o", self.stage / name, obj], check=True)
            self.write(f"lib/{soname}", (self.stage / name).read_bytes())
        self.write_record()
        checksum = bundle.pack_bundle(self.stage, self.output)
        self.assertEqual(bundle.verify_bundle(self.output), checksum)
        (self.stage / "lib/libkrunfw.so.5").unlink()
        with self.assertRaisesRegex(BundleError, "missing the matching SONAME file"):
            bundle.pack_bundle(self.stage, self.root / "missing-alias")


class BuildInputTests(unittest.TestCase):
    @staticmethod
    def commit(root):
        git(root, "add", "--all")
        git(root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "test source")
        commit, tree = git(root, "rev-parse", "HEAD", "HEAD^{tree}").decode().splitlines()
        return {"commit": commit, "tree": tree}

    def test_pinned_gitlinks_lockfiles_and_clean_worktrees(self):
        with tempfile.TemporaryDirectory() as work:
            root = Path(work)
            checkouts = {}
            sources = {}
            for name in ("libkrun", "libkrunfw", "smolvm"):
                worktree = root / (name + "-source")
                worktree.mkdir()
                git(worktree, "init", "-q")
                (worktree / "Cargo.lock").write_text(f"{name} lock\n")
                (worktree / ".gitignore").write_text("target/\n")
                if name == "libkrunfw":
                    archive_digest = sha(b"test archive").split(":")[1]
                    (worktree / "Makefile").write_text(f"KERNEL_VERSION = linux-6.18.60\nKERNEL_SHA256 = {archive_digest}\n")
                if name == "smolvm":
                    (worktree / "src/agent").mkdir(parents=True)
                    (worktree / "src/agent/krun.rs").write_text("load_sym!(krun_add_disk2)\n")
                    for submodule, child in checkouts.items():
                        git(worktree, "-c", "protocol.file.allow=always", "submodule", "add", str(child), submodule)
                sources[name] = self.commit(worktree)
                checkouts[name] = worktree

            smolvm = checkouts["smolvm"]
            archive = root / "kernel.tar.xz"
            archive.write_bytes(b"test archive")
            tools = toolchain()
            lock = build_lock(sources, sha(json.dumps(tools).encode()))
            toolchain_path, lock_path = root / "tools.json", root / "build.json"
            toolchain_path.write_text(json.dumps(tools))
            lock_path.write_text(json.dumps(lock))
            preflight = read_locks(lock_path, toolchain_path)
            check_inputs(smolvm, archive, *preflight)
            with self.assertRaisesRegex(BundleError, "gitlinks do not match"):
                check_checkout(smolvm, sources["smolvm"], {})

            target = smolvm / "libkrun" / "target"
            target.mkdir()
            (target / "stale.so").write_bytes(b"unused build output")
            with self.assertRaisesRegex(BundleError, "build residue"):
                check_inputs(smolvm, archive, *preflight)
            shutil.rmtree(target)

            git(smolvm / "libkrun", "update-index", "--assume-unchanged", "Cargo.lock")
            with self.assertRaisesRegex(BundleError, "unchecked index entry"):
                check_inputs(smolvm, archive, *preflight)
            archive.write_bytes(b"wrong archive")
            with self.assertRaisesRegex(BundleError, "kernel archive digest mismatch"):
                check_inputs(smolvm, archive, *preflight)


class ElfTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("readelf"), "requires GNU readelf")
    def test_checks_architecture_and_static_linking(self):
        binary = Path(shutil.which("true"))
        bundle.elf(binary, {"EXEC", "DYN"})
        result = subprocess.run(["readelf", "-l", str(binary)], capture_output=True, check=True, text=True)
        if "INTERP" in result.stdout:
            with self.assertRaisesRegex(BundleError, "must be statically linked"):
                bundle.elf(binary, {"EXEC", "DYN"}, static=True)


if __name__ == "__main__":
    unittest.main()
