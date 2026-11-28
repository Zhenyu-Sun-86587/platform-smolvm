import json
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import patch



HERE = Path(__file__).resolve().parent
PREFLIGHT = runpy.run_path(str(HERE / "preflight.py"))


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
