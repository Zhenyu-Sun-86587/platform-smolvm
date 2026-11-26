#!/usr/bin/env python3
"""Keep a single TCP stream and a writable volume open across independent service restarts."""

import argparse
import base64
from contextlib import ExitStack, suppress
import hashlib
import json
import os
from pathlib import Path
import select
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
import time

from managed_system_vsock import Platform, attach_for_retirement, wait_for, worker
from worker_restart_kvm import ready_worker, request_fault


PORT = 45628
SERVER = """
set -eu
read -r nonce < /proc/sys/kernel/random/uuid
exec 5>/dev/vdb
sequence=0
while IFS= read -r challenge; do
    sequence=$((sequence + 1))
    reply="$sequence $$ $nonce $challenge"
    printf '%s\n' "$reply" >&5
    sync -d /dev/vdb
    printf '%s\n' "$reply"
done
"""
CLIENT = """
set -eu
mkfifo /run/a20-request
exec 4<>/run/a20-request
exec 3<>"/dev/tcp/$1/$2"
echo ready > /run/a20-client
while IFS= read -r challenge <&4; do
    printf '%s\n' "$challenge" >&3
    IFS= read -r reply <&3
    printf '%s %s\n' "$$" "$reply" >> /run/a20-replies
done
"""


def guest_direct(platform, ref, command):
    """Use the original VMM's local Guest Agent socket even when Serve is offline."""
    body = json.dumps(dict(method="vm_exec", command=command, timeout_ms=10000)).encode()
    with socket.socket(socket.AF_UNIX) as peer:
        peer.settimeout(15)
        peer.connect(str(platform.agent_socket(ref)))
        peer.sendall(struct.pack("!I", len(body)) + body)
        with peer.makefile("rb") as response:
            length, = struct.unpack("!I", response.read(4))
            assert length <= 65536, length
            result = json.loads(response.read(length))
    assert result["status"] == "completed" and result["exit_code"] == 0, result
    return base64.b64decode(result["stdout"])


class RestartPlatform(Platform):
    def spawn(self, command, log):
        process = super().spawn(command, log)
        if log == "serve.log":
            self.serve_process = process
        return process


def guest_machine(platform, name, mode, volume=None):
    intent = dict(name=name, template_id="t_system", boot_mode="system", runtime_instance_id=platform.serve_id,
                  start=volume is None, resources=dict(cpu_count=1, memory_mib=512),
                  network=dict(network_id="stream-net", mode=mode), lifecycle_policy=dict(keep_running=True))
    created = platform.change("machines", "create-" + name, intent)
    platform.complete(created)
    path = "machines/" + created["machine_id"]
    if volume:
        attach_for_retirement(platform, path, volume, name)
    _, ref = platform.context(created["machine_id"])
    wait_for(lambda: platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
    network = platform.app(path + "/network")
    assert network["state"] == "published" and network["mode"] == mode, network
    return created["machine_id"], ref, network["private_ipv4"]


def records(database, sql, value):
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as connection:
        return connection.execute(sql, value if isinstance(value, tuple) else (value,)).fetchall()


def snapshot(platform, guest, heads, tap_state="attached"):
    machine_id, ref, _ = guest
    db = platform.root / "catalog/catalog.sqlite"
    incarnation = ref["incarnation_id"]
    execution, = records(db, """SELECT pid,pid_start_time,host_boot_id,execution_epoch,phase
                                FROM executions WHERE incarnation_id=?""", incarnation)
    pid, start, _, epoch, phase = execution
    proc = Path(f"/proc/{pid}")
    assert (int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19]), epoch, phase) == (
        start, ref["execution_epoch"], "registered"), execution
    cgroup = (proc / "cgroup").read_text().strip()
    assert f"/{os.environ['SMOLVM_WORKER_SLICE_NAME']}/smolvmdeep-vm-{platform.serve_id}-" in cgroup, cgroup
    grant, = records(db, "SELECT grant_id,state FROM writer_grants WHERE incarnation_id=? AND state<>'released'",
                     incarnation)
    assert grant[1] == "attached", grant
    reserved = records(db, """SELECT volume_id,head_resource_id,backing_identity FROM writer_reservations
                              WHERE incarnation_id=? ORDER BY volume_id""", incarnation)
    assert len(reserved) == heads, reserved
    tap, = records(db, """SELECT attachment_id,lease_id,state FROM network_tap_leases
                          WHERE incarnation_id=? AND state<>'retired'""", incarnation)
    assert tap[2] == tap_state, tap
    binding, = records(platform.root / "platform/platform.db", """SELECT m.current_binding_id,b.runtime_name,
        b.generation,b.status FROM machines m JOIN runtime_bindings b ON m.current_binding_id=b.id WHERE m.id=?""",
                       machine_id)
    assert binding[3] == "current", binding
    locked, interfaces = set(), set()
    for entry in (proc / "fdinfo").iterdir():
        with suppress(FileNotFoundError):
            for line in entry.read_text().splitlines():
                if line.startswith("lock:") and "OFDLCK" in line and "WRITE" in line:
                    fd = (proc / "fd" / entry.name).stat()
                    locked.add((fd.st_dev, fd.st_ino))
                if line.startswith("iff:"):
                    interfaces.add(line.split()[1])
    assert {f"inode:{dev}:{ino}" for dev, ino in locked} == {row[2] for row in reserved}, (locked, reserved)
    assert len(interfaces) == 1, interfaces
    return (execution, cgroup, grant[0], tuple(reserved), tap[:2], binding, sorted(locked), sorted(interfaces))


def socket_identity(platform, ref, pid, fd):
    query = """set -eu
        inode=$(readlink "/proc/$1/fd/$2")
        awk -v inode="$inode" '$4=="01" && "socket:["$10"]"==inode {print $2,$3,$10}' /proc/net/tcp
    """
    entry = guest_direct(platform, ref, ["/bin/bash", "-c", query, "socket", pid, str(fd)]).split()
    assert len(entry) == 3, entry
    return entry


def scope_cut(platform, stage, challenge, inventory, baseline):
    created = platform.change("machines", "create-scope-" + stage, dict(
        name="interrupted " + stage, template_id="t_system", boot_mode="system", runtime_instance_id=platform.serve_id,
        start=False, resources=dict(cpu_count=1, memory_mib=512), lifecycle_policy=dict(keep_running=True)))
    platform.complete(created)
    path = "machines/" + created["machine_id"]
    machine = platform.app(path)
    scope = (f"smolvmdeep-vm-{platform.serve_id}-"
             + hashlib.sha256(machine["runtime_name"].encode()).hexdigest()[:32] + ".scope")
    control = platform.root / "scope-cut"
    marker = control / ("seen-" + stage)
    (control / "armed").write_text(json.dumps([scope, stage]))
    op = platform.change(path + "/start", "interrupted-" + stage, {}, machine)
    data = wait_for(lambda: json.loads(marker.read_text()) if marker.exists() else None, timeout=25)
    pid = data["pid"]
    cgroup = Path(f"/proc/{pid}/cgroup").read_text().strip()
    assert (scope in cgroup) == (stage == "after"), cgroup
    assert "_boot-managed" in Path(f"/proc/{pid}/cmdline").read_bytes().decode().split("\0")
    with os.fdopen(os.pidfd_open(pid), "rb", buffering=0) as descriptor:
        parent = platform.serve_process
        assert data["serve"] == parent.pid, data
        parent.kill()
        assert parent.wait(timeout=5) < 0
        (control / "armed").unlink()
        (control / ("release-" + stage)).touch()
        assert select.select([descriptor], [], [], 10)[0], "orphaned VMM after losing Serve's launch pipe"
    db = platform.root / "catalog/catalog.sqlite"
    assert not records(db, "SELECT 1 FROM executions WHERE pid=? AND pid_start_time=?", (pid, data["start"]))
    challenge("scope-" + stage + "-interrupted")
    assert inventory() == baseline
    platform.spawn(parent.args, "serve.log")
    wait_for(lambda: platform.native("/capabilities"), retry=(OSError, RuntimeError))
    platform.complete(op)
    view, ref = platform.context(created["machine_id"])
    assert view["power_state"] == "running"
    wait_for(lambda: platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n", timeout=35,
             retry=(RuntimeError, OSError))
    rows = records(db, "SELECT pid,phase FROM executions WHERE instance_id=?", ref["instance_id"])
    assert len(rows) == 1 and rows[0][0] != pid and rows[0][1] == "registered", rows
    assert len(records(db, "SELECT grant_id FROM writer_grants WHERE incarnation_id=? AND state='attached'",
                       ref["incarnation_id"])) == 1
    print("Interrupted", stage, "StartTransientUnit for PID", pid, "in", cgroup,
          "same start operation recovered with PID", rows[0][0], flush=True)
    view = platform.wait_capability(path, "lifecycle.stop")
    platform.complete(platform.change(path + "/stop", "stop-scope-" + stage, {}, view))


def run(platform):
    platform.start()
    print("Installed bundle", platform.bundle_id, "platform epoch", platform.epoch, flush=True)
    volume = platform.complete(platform.change("volumes", "stream-volume", dict(
        name="Stream receipts", runtime_instance_id=platform.serve_id, capacity_bytes=16 << 20)))["result"]["volume"]
    server = guest_machine(platform, "receiver", "native", volume)
    client = guest_machine(platform, "sender", "fixed_internal")
    for target, name, script in ((server, "receiver", SERVER), (client, "sender", CLIENT)):
        platform.guest(target[1], ["/bin/bash", "-c", 'cat > "$1"', "setup", "/run/a20-" + name], stdin=script)
    platform.guest(server[1], ["systemd-run", "--unit=a20-receiver", "/usr/bin/systemd-socket-activate",
                               "--accept", "--inetd", f"--listen=0.0.0.0:{PORT}", "/bin/bash", "/run/a20-receiver"])
    wait_for(lambda: f":{PORT:04X}".encode() in platform.guest(server[1], ["cat", "/proc/net/tcp"]))
    platform.guest(client[1], ["systemd-run", "--unit=a20-sender", "/bin/bash", "/run/a20-sender",
                               server[2], str(PORT)])
    wait_for(lambda: platform.guest(client[1], ["cat", "/run/a20-client"]) == b"ready\n",
             timeout=25, retry=(RuntimeError, OSError))

    client_pid, server_pid, server_nonce = (None,) * 3
    journal = b""
    sequence = 0
    sockets = []

    def challenge(stage, observe_fence=False):
        nonlocal client_pid, server_pid, server_nonce, journal, sequence, sockets
        sequence += 1
        token = stage + "-" + os.urandom(12).hex()
        command = """printf '%s\n' "$1" > /run/a20-request
            deadline=$((SECONDS + 7))
            until grep -Fq -- "$1" /run/a20-replies 2>/dev/null; do
                if (( SECONDS >= deadline )); then echo unanswered; exit 0; fi
                sleep .05
            done
            tail -n 1 /run/a20-replies"""
        reply = guest_direct(platform, client[1], ["/bin/bash", "-ec", command, "send", token])
        if observe_fence and reply == b"unanswered\n":
            disk = guest_direct(platform, server[1], ["dd", "if=/dev/vdb", "bs=1",
                                                      f"count={len(journal) + 160}", "status=none"])
            assert disk.startswith(journal)
            print(stage, "no reply on the existing TCP stream within seven seconds,",
                  "new challenge present on disk", token.encode() in disk[len(journal):], flush=True)
            return
        client_now, count, server_now, nonce, receipt = reply.decode().split()
        assert (int(count), receipt) == (sequence, token), reply
        if sequence == 1:
            client_pid, server_pid, server_nonce = client_now, server_now, nonce
            assert int(client_pid) > 1 and int(server_pid) > 1
        assert (client_now, server_now, nonce) == (client_pid, server_pid, server_nonce), reply
        pair = [socket_identity(platform, client[1], client_pid, 3), socket_identity(platform, server[1], server_pid, 0)]
        if sequence == 1:
            sockets = pair
            print("Guest-local TCP endpoints and socket inodes", pair, flush=True)
        assert pair == sockets, (pair, sockets)
        journal += reply.split(b" ", 1)[1]
        disk = guest_direct(platform, server[1], ["dd", "if=/dev/vdb", "bs=1",
                                                  f"count={len(journal)}", "status=none"])
        assert disk == journal, (disk, journal)
        print(stage, "TCP reply", reply.decode().strip(), "confirmed volume bytes", len(disk), flush=True)

    def inventory(tap_state="attached"):
        return [snapshot(platform, server, 2, tap_state), snapshot(platform, client, 1, tap_state)]

    baseline = inventory()
    print("Original VMM, scope, writer heads, TAP, binding and open files", json.dumps(baseline), flush=True)
    challenge("baseline")
    if platform.args.scope_cuts:
        for stage in ("before", "after"):
            scope_cut(platform, stage, challenge, inventory, baseline)
    for component in ("platform", "serve", "broker"):
        process = {"platform": platform.platform, "serve": platform.serve_process, "broker": platform.broker}[component]
        previous = worker(platform.worker, "Health")
        process.kill()
        assert process.wait(timeout=15) < 0
        challenge(component + "-down")
        assert inventory() == baseline, component
        replacement = platform.spawn(process.args, component.replace("broker", "worker") + ".log")
        if component == "platform":
            platform.platform = replacement
            assert wait_for(lambda: platform.app("control"))["control_epoch"] == platform.epoch
        elif component == "serve":
            handshake = wait_for(lambda: platform.native("/capabilities"), retry=(OSError, RuntimeError))
            assert (handshake["runtime_instance_id"], handshake["bundle_digest"]) == (platform.serve_id, platform.bundle_id)
        else:
            platform.broker = replacement
            if platform.args.expect_unrecovered_network:
                def fenced():
                    health = worker(platform.worker, "Health")
                    return health if (health["state"] == "recovering" and
                                      previous["broker_session_id"] != health["broker_session_id"]) else None
                wait_for(fenced, retry=(OSError, RuntimeError))
                # Allow multiple housekeeping cycles to recover the two live writers.
                wait_for(lambda: inventory("unknown") == baseline, retry=(AssertionError,))
                challenge("broker-recovering", observe_fence=True)
                time.sleep(3)
                assert fenced() and inventory("unknown") == baseline
                request_fault("E_RECOVERING", lambda: worker(platform.worker, "CompleteRecovery", {
                    "broker_session_id": fenced()["broker_session_id"]}))
                print("Original broker recovered both writers. Both original TAP leases stayed unknown,",
                      "and completion of broker recovery was fenced.", flush=True)
                return
            ready_worker(platform, platform.bundle / "smolvm-resourced", previous)
        for machine_id, original, address in (server, client):
            wait_for(lambda: platform.context(machine_id)[1] == original, retry=(OSError, RuntimeError))
            status = platform.app(f"machines/{machine_id}/network")
            assert (status["state"], status["private_ipv4"]) == ("published", address), status
        assert inventory() == baseline, component
        challenge(component + "-restored")
        print(component, "PID", process.pid, "->", replacement.pid, flush=True)


def main():
    options = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        options.add_argument("--" + name, type=lambda path: Path(path).resolve(), required=True)
    options.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    options.add_argument("--expect-unrecovered-network", action="store_true",
                         help="reproduce the frozen broker's two unknown live TAP leases")
    options.add_argument("--scope-cuts", action="store_true",
                         help="interrupt Serve just before and after systemd moves a new VMM into its scope")
    args = options.parse_args()
    if os.geteuid() != 0:
        options.error("requires Linux KVM and root on a systemd host")
    if os.environ.get("COMPONENT_RESTART_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "COMPONENT_RESTART_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a20-") as temp, ExitStack() as stack:
        root = Path(temp)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=7G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "-l", root], check=True)
        name = f"smolvmrestart{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        name, "fail", "1", "MemoryAccounting", "b", "true", "0"], check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = name
        if args.scope_cuts:
            control = root / "scope-cut"
            control.mkdir()
            original = control / "busctl"
            original.touch()
            subprocess.run(["mount", "--bind", "/usr/bin/busctl", original], check=True)
            stack.callback(subprocess.run, ["umount", original], check=True)
            hook = Path(__file__).resolve().parents[1] / "fixtures/restart-scope-cut.py"
            subprocess.run(["mount", "--bind", hook, "/usr/bin/busctl"], check=True)
            stack.callback(subprocess.run, ["umount", "/usr/bin/busctl"], check=True)
            os.environ["A20_SCOPE_CUT"] = str(control)
        network = dict(network_id="stream-net", route_domain_id="private-stream", cidr="10.32.70.0/24")
        platform = RestartPlatform(args, root, stack, (network,), use_installation=True)
        try:
            run(platform)
        except BaseException:
            for log in (root / "platform.log", root / "serve.log", root / "worker.log"):
                if log.exists():
                    print(log.name, log.read_text(errors="replace")[-4000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
