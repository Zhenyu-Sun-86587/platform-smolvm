#!/usr/bin/env python3
"""Restore ordinary historical StatePoints with two installed Bundle readers."""

import argparse
from contextlib import ExitStack, closing
import json
import os
from pathlib import Path
import secrets
import sqlite3
import subprocess
import sys
import tempfile

from lifecycle_wake_collector_kvm import readonly_bind
from managed_system_vsock import EXT, Platform, UnixHTTPConnection, digest, request, wait_for, worker
from worker_gc_kvm import backup_file


NETWORK = {"network_id": "a19-fixed", "route_domain_id": "a19-lab", "cidr": "10.86.19.0/24"}
ROOT = Path(__file__).resolve().parents[2]


def reader(endpoint):
    return wait_for(lambda: request(UnixHTTPConnection(endpoint), "GET", EXT + "/capabilities")[0],
                    retry=(OSError, RuntimeError))


class HistoryPlatform(Platform):
    def spawn(self, command, log):
        if log == "worker.log":
            command = [self.args.worker or self.current / "smolvm-resourced", *command[1:]]
        if log == "platform.log" and not hasattr(self, "routes"):
            self.config = Path(command[-1])
            self.current_socket = self.root / "peer-api/serve.sock"
            super().spawn(["/usr/bin/env", f"LD_LIBRARY_PATH={self.current / 'lib'}",
                           f"SMOLVM_LIB_DIR={self.current / 'lib'}", self.current / "smolvm-bin",
                           "--state-dir", self.root / "peer-state", "serve", "start", "--listen",
                           f"unix://{self.current_socket}", "--worker-resource-socket", self.worker,
                           "--guest-rollout-port", "37452", "--shutdown-grace", "0"], "peer.log")
            self.current_reader = reader(self.current_socket)
            config = json.loads(self.config.read_text())
            self.original_route, = config["runtime"]["custom"]
            self.current_route = {**self.original_route, "instance_id": self.current_reader["runtime_instance_id"],
                                  "bundle_digest": self.current_reader["bundle_digest"],
                                  "state_reader_id": self.current_reader["state_reader_id"],
                                  "socket": str(self.current_socket), "manifest": str(self.current / "runtime-bundle.json"),
                                  "private_networks": []}
            self.routes = [self.original_route, self.current_route]
            config["runtime"]["custom"] = self.routes
            self.config.write_text(json.dumps(config))
        process = super().spawn(command, log)
        if log == "serve.log":
            self.original_serve = process
            self.original_command = command
        return process

    def configure(self, routes):
        self.platform.terminate()
        self.platform.wait(timeout=15)
        config = json.loads(self.config.read_text())
        config["runtime"]["custom"] = routes
        self.config.write_text(json.dumps(config))
        self.platform = self.spawn(self.platform_command, "platform.log")
        self.epoch = wait_for(lambda: self.app("control"))["control_epoch"]

    def cli(self, *args, failure=False):
        result = subprocess.run([self.args.sandboxctl, "--json", *args],
                                env={**self.env, "SANDBOX_TOKEN": self.api_token, "SANDBOX_URL": self.origin},
                                text=True, capture_output=True, timeout=190)
        assert (result.returncode != 0) == failure, (args, result.returncode, result.stdout, result.stderr)
        if failure and not result.stdout:
            return json.loads(result.stderr.splitlines()[-1])
        assert result.stdout, (args, result.stderr)
        return json.loads(result.stdout)

    def read_peer(self, path):
        return request(UnixHTTPConnection(self.current_socket), "GET", EXT + path)[0]


def inventory(platform):
    catalog = platform.root / "catalog/catalog.sqlite"
    with closing(sqlite3.connect(catalog.as_uri() + "?mode=ro", uri=True)) as db:
        return db.execute("SELECT instance_id,incarnation_id,phase FROM executions ORDER BY instance_id,incarnation_id").fetchall()


def assert_reader(bundle, handshake):
    manifest = json.loads((bundle / "runtime-bundle.json").read_bytes())
    assert handshake["bundle_digest"] == manifest["bundle_digest"], handshake
    assert handshake["state_reader_id"] == manifest["contracts"]["state_reader_id"], handshake
    print(f"Registered Bundle {handshake['bundle_digest']} reader {handshake['state_reader_id']}.", flush=True)
    return manifest


def machine(platform, name, route, cpu, memory, *flags):
    return platform.cli("machines", "create", name, "--template", "t_system", "--boot-mode", "system",
                        "--runtime-instance", route["instance_id"], "--cpu", str(cpu),
                        "--memory-mib", str(memory), "--idle=false", *flags)["machine_id"]


def witness(platform, execution):
    return platform.guest(execution, ["/bin/sh", "-ec", r'''
        cat /run/a19/marker /var/tmp/a19-disk-marker
        pid=$(systemctl show --value -p MainPID a19-history-counter.service)
        test "$pid" -gt 1
        started=$(awk '{print $22}' "/proc/$pid/stat")
        printf '%s/%s\n' "$pid" "$started"
        cat /run/a19/count
    ''']).decode().splitlines()


def archive_copy(platform, captures, old_manifest):
    backup = "a19-immutable-copy"
    exact_reader = old_manifest["contracts"]["state_reader_id"]
    receipt, manifest_file = backup_file(platform, "PrepareBackupClosure", {
        "backup_id": backup, "capture_ids": sorted(captures), "generation_ids": [],
        "state_reader_id": exact_reader, "bundle_manifest": old_manifest,
    }, "backup_manifest")
    with manifest_file:
        raw = manifest_file.read()
    closure = json.loads(raw)
    assert {item["point"]["capture_id"]: item["manifest_digest"] for item in closure["captures"]} == captures, closure
    binary = platform.args.worker or platform.current / "smolvm-resourced"
    command = [binary, "recover-backup-closure", "--bundle-digest", platform.bundle_id,
               "--state-reader-id", exact_reader, "--manifest-digest", receipt["manifest_digest"], "--archive"]

    def verify(path, failure=False):
        result = subprocess.run([*command, path], env=platform.env, text=True, capture_output=True, timeout=170)
        assert (result.returncode != 0) == failure, (result.stdout, result.stderr)
        return result.stderr if failure else json.loads(result.stdout)

    try:
        with tempfile.TemporaryDirectory(dir=platform.root, prefix="restore-copy-") as scratch:
            archive = Path(scratch) / "complete"
            archive.mkdir()
            (archive / "closure.json").write_bytes(raw)
            for name, size in sorted(closure["members"].items()):
                target = archive / name
                target.parent.mkdir(parents=True, exist_ok=True)
                exported, file = backup_file(platform, "ReadBackupMember", {
                    "backup_id": backup, "member": name,
                }, "backup_member")
                with file:
                    assert os.fstat(file.fileno()).st_size == size, exported
                    subprocess.run(["cp", "--sparse=always", f"/proc/self/fd/{file.fileno()}", target],
                                   check=True, pass_fds=(file.fileno(),))
            healthy = verify(archive)
            assert healthy["captures"] == len(captures), healthy
            copy = Path(scratch) / "copy"
            subprocess.run(["cp", "-a", "--sparse=always", "--reflink=auto", archive, copy], check=True)
            member = Path(min(name for name, size in closure["members"].items()
                              if name.startswith("statepoints/") and "objects" in Path(name).parts and size > 4096))
            removed = Path(scratch) / "member"
            (copy / member).rename(removed)
            missing = verify(copy, failure=True)
            assert "no such file" in missing.lower() or "not exist" in missing.lower(), missing
            removed.rename(copy / member)
            with (copy / member).open("r+b") as file:
                file.truncate(os.fstat(file.fileno()).st_size - 1)
            truncated = verify(copy, failure=True)
            assert any(term in truncated.lower() for term in ("length", "size", "checksum", "digest", "unexpected")), truncated
            assert verify(archive) == healthy
            print(f"Backup {backup} reopened with both original closures, manifest {receipt['manifest_digest']}. "
                  f"An independent copy with missing or shortened StatePoint member {member.name} was rejected.", flush=True)
    finally:
        worker(platform.worker, "ReleaseBackupClosure", {"backup_id": backup}, route_id="deep", bundle_digest=platform.bundle_id)


def run(platform):
    old = platform.native("/capabilities")
    old_manifest = assert_reader(platform.bundle, old)
    current = assert_reader(platform.current, platform.current_reader)
    assert old["bundle_digest"] != current["bundle_digest"]
    worker_sha = (digest(platform.args.worker) if platform.args.worker else "sha256:" + next(
        asset["sha256"] for asset in current["artifacts"] if asset["path"] == "smolvm-resourced"))
    assert worker(platform.worker, "Health")["executable_sha256"] == worker_sha
    credential = platform.change("tokens", "a19-owner-cli", {"label": "A19 history", "scope": {"access": "manage"}}, status=200)
    platform.api_token = credential["token"]
    print(f"Shared Worker {worker_sha}; platformd {digest(platform.args.platformd)}; "
          f"sandboxctl {digest(platform.args.sandboxctl)}.", flush=True)

    source = machine(platform, "Archived FixedInternal source", platform.original_route, 1, 512,
                     "--network", NETWORK["network_id"], "--network-mode", "fixed_internal")
    peer = machine(platform, "Current independent resource shape", platform.current_route, 2, 640)
    _, origin = platform.context(source)
    peer_view = platform.app("machines/" + peer)
    peer_execution = platform.read_peer(f"/machines/{peer_view['runtime_name']}/execution")
    assert origin["instance_id"] != peer_execution["instance_id"]
    original_pids = {row["instance_id"]: row["pid"] for row in (
        platform.native(f"/instances/{origin['instance_id']}/inventory")["execution"],
        platform.read_peer(f"/instances/{peer_execution['instance_id']}/inventory")["execution"],
    )}
    assert len(set(original_pids.values())) == 2
    print(f"Both immutable readers booted separate KVM processes: {original_pids}.", flush=True)

    platform.guest(origin, ["/usr/bin/mkdir", "-p", "/run/a19"])
    platform.guest(origin, ["/usr/bin/systemd-run", "--quiet", "--unit=a19-history-counter", "/bin/sh", "-ec", r'''
        count=0
        while :; do
            count=$((count + 1))
            printf '%s\n' "$count" > /run/a19/next
            mv /run/a19/next /run/a19/count
            sleep .2
        done
    '''])
    saved, captures = [], {}
    for index in range(2):
        marker = f"a19-point-{index}-{secrets.token_hex(8)}"
        platform.guest(origin, ["/bin/sh", "-ec", 'printf "%s\n" "$1" > /run/a19/marker; '
            'printf "%s\n" "$1" > /var/tmp/a19-disk-marker; sync /var/tmp/a19-disk-marker', "write", marker])
        first = wait_for(lambda: witness(platform, origin), retry=(RuntimeError, OSError))
        assert first[:2] == [marker, marker], first
        response = platform.cli("snapshots", "capture", source, "--capture-mode", "vm", "--label", marker)["result"]
        assert response["statepoint"]["phase"] == "durable", response
        saved.append((response["snapshot_id"], response["statepoint"]["manifest_digest"], first))
        captures[response["statepoint"]["capture_id"]] = response["statepoint"]["manifest_digest"]
    assert saved[0][1] != saved[1][1] and saved[0][2][2] == saved[1][2][2]
    print(f"Archived reader captured two live FixedInternal cuts {[point[1] for point in saved]}.", flush=True)
    if platform.args.verify_backup_copy:
        archive_copy(platform, captures, old_manifest)

    platform.original_serve.terminate()
    platform.original_serve.wait(timeout=15)
    platform.configure([platform.current_route])
    old_ref = platform.cli("snapshots", "get", saved[0][0])
    assert old_ref["state"] == "ready", old_ref
    before = inventory(platform)
    unavailable = platform.cli("snapshots", "restore", saved[0][0], "Missing historic route", failure=True)
    assert unavailable["error"]["code"] == "CAPABILITY_UNKNOWN" and unavailable["error"]["effect"] == "none", unavailable
    assert inventory(platform) == before, unavailable
    assert platform.read_peer(f"/machines/{peer_view['runtime_name']}/execution") == peer_execution
    references = subprocess.run([sys.executable, ROOT / "packaging/deep/bundle-history.py", "references",
                                 "--registry", platform.root / "platform/platform.db", "--worker-catalog",
                                 platform.root / "catalog/catalog.sqlite", "--bundle", old["bundle_digest"]],
                                text=True, capture_output=True, timeout=15)
    assert references.returncode == 0, references.stderr
    pins = json.loads(references.stdout)
    assert pins["worker"]["durable_or_exporting_statepoints"] == 2 and not pins["can_retire"], pins
    print(f"Unavailable historical reader returned {unavailable['error']['code']}; "
          f"the new reader kept its execution and both StatePoints retained their references.", flush=True)

    platform.spawn(platform.original_command, "serve.log")
    assert_reader(platform.bundle, reader(platform.serve))
    platform.configure(platform.routes)
    for index, (snapshot, manifest, first) in enumerate(saved):
        response = platform.cli("snapshots", "restore", snapshot, f"Restored historic point {index}")
        assert response["result"]["statepoint"]["manifest_digest"] == manifest, response
        ident = response["machine_id"]
        _, execution = platform.context(ident)
        assert execution["instance_id"] not in original_pids and execution["execution_epoch"] == 1, execution
        state = wait_for(lambda: witness(platform, execution), retry=(RuntimeError, OSError))
        assert state[:3] == first[:3] and int(state[3]) >= int(first[3]), (first, state)
        later = wait_for(lambda: (counter := witness(platform, execution))[3] != state[3] and counter,
                         retry=(RuntimeError, OSError))
        assert later[:3] == first[:3] and int(later[3]) > int(state[3]), later
        network = platform.cli("machines", "network", ident)
        assert network["mode"] == "fixed_internal" and network["network_id"] == NETWORK["network_id"], network
        print(f"Restored original reader point {manifest} with {network['mode']}, Guest task PID/start {state[2]}, "
              f"RAM and disk marker {state[0]}, and an advancing counter {state[3]} to {later[3]}.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "current", "platformd", "sandboxctl", "system-image"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--worker", type=lambda value: Path(value).resolve(strict=True), help="explicit development Worker binary")
    parser.add_argument("--verify-backup-copy", action="store_true", help="check missing and shortened data on an independent backup copy")
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("A19_HISTORY_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                            sys.executable, __file__, *sys.argv[1:]], {**os.environ, "A19_HISTORY_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="a19-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=5G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmhistory{os.getpid()}_{secrets.token_hex(4)}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        platform = HistoryPlatform(args, root, stack, networks=[NETWORK], state_tmpfs=True, use_installation=True)
        platform.current = root / "current"
        platform.current.mkdir()
        readonly_bind(stack, args.current, platform.current)
        try:
            platform.start()
            run(platform)
        except BaseException:
            for log in (*root.glob("*.log"), *(root / "state/managed").glob("*.log")):
                print(log.name, log.read_text(errors="replace")[-2000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
