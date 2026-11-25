import json
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import patch


HERE = Path(__file__).resolve().parent
SOURCE = runpy.run_path(str(HERE / "source-checkout.py"))
PREFLIGHT = runpy.run_path(str(HERE / "preflight.py"))


def git(path, *args):
    return SOURCE["git"](path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", *args,
                         protocol="file")


class SourceTest(unittest.TestCase):
    def test_offline_checkout_and_downstream_diff(self):
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            mirrors = work / "mirrors"
            mirrors.mkdir()
            sources = {}
            for name in ("libkrun", "libkrunfw", "smolvm", "platform-smolvm"):
                repo = work / name
                repo.mkdir()
                git(repo, "init", "--quiet")
                (repo / "README").write_text(name)
                git(repo, "add", "README")
                if name == "smolvm":
                    for dependency in ("libkrun", "libkrunfw"):
                        path = f"dependencies/{dependency}"
                        key = f"submodule.{dependency}"
                        git(repo, "config", "--file=.gitmodules", f"{key}.path", path)
                        git(repo, "config", "--file=.gitmodules", f"{key}.url", f"git@github.com:smol-machines/{dependency}.git")
                        git(repo, "config", "--file=.gitmodules", f"{key}.branch", "main")
                        git(repo, "update-index", "--add", "--cacheinfo", f"160000,{sources[dependency]['commit']},{path}")
                    git(repo, "add", ".gitmodules")
                git(repo, "commit", "-m", name)
                sources[name] = {"origin": f"https://github.com/example/{name}", "commit": git(repo, "rev-parse", "HEAD")}
                if name != "platform-smolvm":
                    sources[name]["upstream"] = f"https://github.com/smol-machines/{name}"
                if name == "libkrunfw":
                    git(repo, "commit", "--allow-empty", "-m", "Observed newer firmware")
                    sources[name].update(observed=git(repo, "rev-parse", "HEAD"), tree=git(repo, "rev-parse", "HEAD^{tree}"))
                git(work, "clone", "--bare", "--quiet", str(repo), str(mirrors / f"{name}.git"))

            target = work / "isolated"
            checkout = SOURCE["checkout"](target, sources, mirrors, None)
            self.assertEqual(SOURCE["verify"](target, sources), checkout)
            self.assertEqual(checkout["libkrunfw"]["gitlink"], "dependencies/libkrunfw")
            with self.assertRaises(FileExistsError):
                SOURCE["checkout"](target, sources, mirrors, None)
            dep = target / "smolvm/dependencies/libkrunfw"
            (dep / "dirty").touch()
            with self.assertRaisesRegex(ValueError, "local changes"):
                SOURCE["verify"](target, sources)
            (dep / "dirty").unlink()
            diff = SOURCE["prepare_downstream"](target, sources, "deep-runtime/main")
            self.assertIn("https://github.com/example/libkrunfw", diff["runtime_diff"])
            self.assertNotIn("+\tbranch", diff["runtime_diff"])
            self.assertEqual(git(dep, "rev-parse", "HEAD"), sources["libkrunfw"]["commit"])
            self.assertEqual(git(dep, "branch", "--show-current"), "deep-runtime/main")
            self.assertEqual(git(target / "smolvm", "diff", "--name-only"), ".gitmodules")


class PreflightTest(unittest.TestCase):
    def setUp(self):
        self.inventory = json.loads((HERE / "isolation.example.json").read_text())
        self.deep, self.official, self.cocoon = self.inventory["routes"]

    def test_inventory_clear(self):
        self.assertEqual(PREFLIGHT["run"](self.inventory, True)["status"], "clear")

    def test_conflicts_include_alias_and_second_pool(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shared = root / "objects"
            shared.mkdir()
            alias = root / "alias"
            alias.symlink_to(shared)
            self.deep["paths"]["worker_state"] = str(alias / "deep-state")
            self.official["paths"]["runtime_state"] = str(shared)
            self.deep["units"].append("smolvm-status@other.service")
            self.deep["slices"] = ["smolvm-deep.slice"]
            self.deep["ports"][0] = {"protocol": "tcp", "bind": "::", "range": [21100, 21100]}
            self.deep["address_pools"].extend(["192.168.200.0/24", "100.80.0.0/24"])
            self.official["address_pools"] = ["192.168.200.128/25"]
            report = PREFLIGHT["run"](self.inventory, True)
        self.assertEqual(report["status"], "conflict")
        messages = "\n".join(report["conflicts"])
        for message in ("overlaps official runtime_state", "conflicts with official unit smolvm-status@.service",
                        "conflicts with official slice smolvm.slice", "conflicts with deep tcp",
                        "overlaps official 192.168.200.128/25", "overlaps Tailscale"):
            self.assertIn(message, messages)

    def test_zero_status_with_netlink_denial_is_unknown(self):
        denial = subprocess.CompletedProcess([], 0, "", "Cannot open netlink socket: Operation not permitted\n")
        with patch("subprocess.run", return_value=denial):
            report = PREFLIGHT["run"](self.inventory, False)
        self.assertEqual(report["status"], "unknown")
        self.assertTrue(any("bound_inet_sockets" in error for error in report["unknown"]))
        self.assertNotIn("bound_inet_sockets", report["host"])


if __name__ == "__main__":
    unittest.main()
