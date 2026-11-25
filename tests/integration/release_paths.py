"""Exercise the installer's read-only archive and release path checks."""
import hashlib
import io
import json
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest


REPO = pathlib.Path(__file__).resolve().parents[2]
INSTALLER = REPO / "packaging/install/install.sh"
MEMBERS = ("bin/platformd", "bin/sandboxctl", "runtime-manifest.json", "systemd/platformd.service",
           "systemd/smolvm-serve.service", "systemd/smolvm-status.socket", "systemd/smolvm-status@.service",
           "systemd/grant-runtime-socket", "systemd/read-runtime-status", "install.sh", "config.example.yaml",
           "managed-template.tar", "templates.json")


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def add(archive, path, value):
    entry = tarfile.TarInfo(path)
    entry.size = len(value)
    archive.addfile(entry, io.BytesIO(value))


class ReleasePathsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="release-paths-test-")
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.release = self.root / "release"
        self.release.mkdir()
        for path in MEMBERS:
            f = self.release / path
            f.parent.mkdir(parents=True, exist_ok=True)
            f.touch()
        shutil.copy(REPO / "packaging/runtime-manifest.json", self.release / "runtime-manifest.json")
        shutil.copy(INSTALLER, self.release / "install.sh")
        for name in ("platformd", "sandboxctl"):
            executable = self.release / "bin" / name
            executable.write_text("#!/bin/sh\nexit 0\n")
            executable.chmod(0o755)

    def template(self, bad_path=None, link=False):
        path = self.release / "managed-template.tar"
        with tarfile.open(path, "w:") as archive:
            add(archive, "manifest.json", b'[{"Config":"config.json","Layers":["layer.tar"]}]')
            add(archive, "config.json", b"{}")
            add(archive, "layer.tar", b"layer")
            if bad_path:
                add(archive, bad_path, b"malformed fixture")
            if link:
                entry = tarfile.TarInfo("invalid-link")
                entry.type = tarfile.SYMTYPE
                entry.linkname = "config.json"
                archive.addfile(entry)
        digest = sha(path)
        (self.release / "templates.json").write_text(json.dumps([{"image_digest": "sha256:" + digest,
            "spec": {"local_image": "debian-managed-" + digest[:12] + ".tar"}}]))
        manifest = {"format": 1, "version": "0.1.0", "schema_version": 2, "runtime_version": "1.37.1",
                    "architecture": "amd64", "files": [{"path": f, "sha256": sha(self.release / f)} for f in MEMBERS]}
        (self.release / "release-manifest.json").write_text(json.dumps(manifest))

    def plan(self, *args):
        return subprocess.run([str(INSTALLER), "--plan", "--manifest", str(self.release / "release-manifest.json"),
                               "--config", str(self.release / "config.example.yaml"), *args],
                              capture_output=True, text=True, check=False)

    def test_rejects_outer_template_paths_and_links(self):
        for path in ("/invalid-test-entry", "../invalid-test-entry"):
            with self.subTest(path=path):
                self.template(bad_path=path)
                outcome = self.plan()
                self.assertNotEqual(outcome.returncode, 0)
                self.assertIn("invalid member path", outcome.stderr)
        self.template(link=True)
        outcome = self.plan()
        self.assertNotEqual(outcome.returncode, 0)
        self.assertIn("regular files or directories", outcome.stderr)

    def test_rejects_relocated_binary_parent(self):
        self.template()
        (self.release / "bin").rename(self.root / "relocated")
        (self.release / "bin").symlink_to(self.root / "relocated", target_is_directory=True)
        outcome = self.plan()
        self.assertNotEqual(outcome.returncode, 0)
        self.assertIn("Invalid release file bin/platformd", outcome.stderr)

    def test_rejects_unpinned_runtime_archive(self):
        self.template()
        runtime = self.root / "smolvm-1.37.1-linux-x86_64.tar.gz"
        with tarfile.open(runtime, "w:gz") as archive:
            add(archive, "nonrelease-member", b"test fixture")
        outcome = self.plan("--runtime-archive", str(runtime))
        self.assertNotEqual(outcome.returncode, 0)
        self.assertIn("Official runtime archive checksum mismatch", outcome.stderr)


if __name__ == "__main__":
    unittest.main()
