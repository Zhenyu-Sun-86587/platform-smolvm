import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


INSTALLER = Path(__file__).resolve().parents[2] / "guest/templates/system-debian/install.sh"
NETWORK = Path("etc/systemd/network/05-smolvm-uplink.network")


class SystemTemplateTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        self.root = self.work / "root"
        self.binaries = self.work / "binaries"
        self.outside = self.work / "outside"
        for directory in (self.root / "usr/lib/systemd", self.root / "etc", self.binaries, self.outside):
            directory.mkdir(parents=True)
        for name in ("bin", "sbin", "lib"):
            (self.root / name).symlink_to(f"usr/{name}")
        (self.root / "usr/lib/os-release").write_text("ID=debian\n")
        (self.root / "etc/os-release").symlink_to("../usr/lib/os-release")
        for path in (self.root / "usr/lib/systemd/systemd", *(
            self.binaries / name for name in ("smolvm-agent", "taskd", "taskctl", "task-runner")
        )):
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
        self.sentinel = self.outside / "sentinel"
        self.sentinel.write_text("untouched")

    def install(self, root):
        return subprocess.run([INSTALLER, root, self.binaries, "smol-uplink"], capture_output=True, text=True)

    def test_escaping_paths_are_rejected_before_writing(self):
        for i, (path, target) in enumerate((
            ("usr/local/bin", self.outside),
            ("etc/systemd", self.outside),
            (NETWORK, self.sentinel),
            (NETWORK, "../../../../outside/sentinel"),
        )):
            with self.subTest(path=path, target=target):
                guest = shutil.copytree(self.root, self.work / f"guest-{i}", symlinks=True)
                destination = guest / path
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.symlink_to(target)
                result = self.install(guest)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("path escapes the Debian rootfs", result.stderr)
                self.assertFalse((guest / "usr/local/bin/smolvm-agent").exists())
                self.assertEqual(list(self.outside.iterdir()), [self.sentinel])
                self.assertEqual(self.sentinel.read_text(), "untouched")

    def test_host_root_alias_is_rejected(self):
        alias = self.work / "alias"
        alias.symlink_to("/")
        self.assertEqual(self.install(alias).returncode, 2)

    def test_merged_usr_template_can_be_installed_twice(self):
        for _ in range(2):
            result = self.install(self.root)
            self.assertEqual(result.returncode, 0, result.stderr)
            units = self.root / "etc/systemd/system"
            for name in ("smolvm-agent.service", "sandbox-taskd.service"):
                path = units / "multi-user.target.wants" / name
                self.assertEqual(path.resolve(), units / name)
            self.assertEqual(
                (self.root / NETWORK).read_text(),
                "[Match]\nName=smol-uplink\n\n[Link]\nUnmanaged=yes\n",
            )


if __name__ == "__main__":
    unittest.main()
