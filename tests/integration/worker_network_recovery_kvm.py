#!/usr/bin/env python3
"""Verify DNS, scoped service dialing, and router pin retirement on Linux/KVM."""

import argparse
import array
from contextlib import ExitStack, closing
import json
import os
from pathlib import Path
import select
import socket
import sqlite3
import subprocess
import sys
import tempfile

from component_restart_kvm import guest_machine
from managed_system_vsock import Platform, wait_for, worker
from worker_restart_kvm import ready_worker, request_fault, restart_worker, worker_scope


PORT = 45710
SERVER = """
set -eu
IFS= read -r request
printf '%s\n' "$request" >> /run/a20-business
printf '%s\n' "$request"
"""


def query_attachment(platform, execution):
    return worker(platform.worker, "QueryExecutionNetworkAttachment",
                  **worker_scope(platform, execution))


def connect_service(platform, execution, current):
    lease = current["lease"]
    payload = dict(attachment_id=lease["attachment"]["attachment_id"],
                   lease_id=lease["lease_id"], expected_revision=lease["revision"],
                   guest_port=PORT, transport="tcp")
    packet = dict(protocol="WorkerResource/1", method="DialNetworkAttachment",
                  request_id="fixture-service", payload=payload,
                  operation_id=lease["operation_id"], request_digest=current["request_digest"],
                  **worker_scope(platform, execution))
    descriptors = array.array("i")
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.settimeout(10)
        connection.connect(str(platform.worker))
        connection.sendall(json.dumps(packet).encode())
        raw, ancillary, flags, _ = connection.recvmsg(65536, socket.CMSG_SPACE(descriptors.itemsize))
    assert not flags & socket.MSG_CTRUNC
    result = json.loads(raw)
    if "error" in result:
        assert not ancillary, result
        raise RuntimeError(result["error"])
    for level, kind, data in ancillary:
        assert (level, kind) == (socket.SOL_SOCKET, socket.SCM_RIGHTS)
        descriptors.frombytes(data)
    assert result["result"] == lease, result
    assert result["fd_table"] == [dict(index=0, role="network_tcp",
                                        resource_id=payload["attachment_id"])], result
    fd, = descriptors
    return socket.socket(fileno=fd)


def business_request(platform, execution, target, label):
    # Tokens are checked in the actual Guest after both restarts.
    receipt = f"{label}-{os.urandom(12).hex()}"
    with connect_service(platform, execution, target) as service:
        assert select.select([], [service], [], 5)[1], "scoped TCP dial did not connect"
        assert service.getsockopt(socket.SOL_SOCKET, socket.SO_ERROR) == 0
        service.settimeout(5)
        service.sendall(receipt.encode() + b"\n")
        with service.makefile("rb") as response:
            assert response.readline() == receipt.encode() + b"\n"
    return receipt


def in_router(pin, *command, **options):
    return subprocess.run(["nsenter", f"--net={pin}", *command], check=True, **options)


def read_journal(platform, query):
    path = platform.root / "catalog/catalog.sqlite"
    with closing(sqlite3.connect(f"file:{path}?mode=ro", uri=True)) as database:
        return database.execute(query).fetchall()


def run(platform):
    platform.start()
    print("Installed bundle", platform.bundle_id, flush=True)
    native = guest_machine(platform, "a20-native", "native")
    fixed = guest_machine(platform, "a20-fixed", "fixed_internal")
    guests = [native, fixed]
    for _, execution, _ in guests:
        platform.guest(execution, ["systemd-run", "--unit=a20-service",
                                   "/usr/bin/systemd-socket-activate", "--accept", "--inetd",
                                   f"--listen=0.0.0.0:{PORT}", "/bin/bash", "-ec", SERVER])
        wait_for(lambda: f":{PORT:04X}".encode() in platform.guest(execution, ["cat", "/proc/net/tcp"]))

    pin_root = platform.root / "catalog/network-namespaces"
    pin, = pin_root.iterdir()
    original_ns = pin.stat()
    old = [query_attachment(platform, execution) for _, execution, _ in guests]
    assert all(entry["lease"]["state"] == "attached" and entry["lease"]["gate"] == "open" for entry in old)

    def dns_pair():
        for source, target, address in [(native, old[1], fixed[2]), (fixed, old[0], native[2])]:
            reply = platform.guest(source[1], ["getent", "ahostsv4", target["dns_name"]])
            assert {line.decode().split()[0] for line in reply.splitlines()} == {address}, reply

    def current():
        return [query_attachment(platform, execution) for _, execution, _ in guests]

    def assert_original_ports(before, after):
        for prior, live in zip(before, after, strict=True):
            p, r = prior["lease"], live["lease"]
            assert (r["state"], r["gate"]) == ("attached", "open"), live
            assert r["revision"] > p["revision"], (prior, live)
            for field in ("lease_id", "attachment", "incarnation_id", "execution_epoch", "broker_session_id"):
                assert r[field] == p[field], (field, prior, live)
        assert (pin.stat().st_dev, pin.stat().st_ino) == (original_ns.st_dev, original_ns.st_ino)
        assert list(pin_root.iterdir()) == [pin]

    def assert_business(expected):
        for (_, execution, _), results in zip(guests, expected, strict=True):
            assert platform.guest(execution, ["cat", "/run/a20-business"]).decode().splitlines() == results

    binary = platform.bundle / "smolvm-resourced"
    dns_pair()
    before = [[business_request(platform, execution, entry, "before")]
              for (_, execution, _), entry in zip(guests, old, strict=True)]
    previous = worker(platform.worker, "Health")
    restart_worker(platform, binary)
    recovered = ready_worker(platform, binary, previous)
    attached = current()
    assert_original_ports(old, attached)
    dns_pair()
    for (_, execution, _), entry in zip(guests, old, strict=True):
        request_fault("E_STALE_REVISION", lambda: connect_service(platform, execution, entry))
    assert_business(before)
    for receipts, (_, execution, _), entry in zip(before, guests, attached, strict=True):
        receipts.append(business_request(platform, execution, entry, "after"))
    assert_business(before)
    print("Original router namespace", original_ns.st_dev, original_ns.st_ino,
          "resolved both original DNS labels, rejected stale lease revisions and routed one new request each", flush=True)

    # Alter a rule in this fixture's private router, on an unused port.
    bad_table = "a20_fixture_fault"
    in_router(pin, "nft", "-f", "-", input=f"""
      add table ip {bad_table}
      add chain ip {bad_table} check {{ type filter hook forward priority 1; policy accept; }}
      add rule ip {bad_table} check tcp dport 1 drop
    """, text=True)
    restart_worker(platform, binary)

    def fenced():
        health = worker(platform.worker, "Health")
        scope = health["state"] == "recovering" and health["broker_session_id"] != recovered["broker_session_id"]
        journal = read_journal(platform, "SELECT state FROM network_tap_leases WHERE state <> 'retired'")
        log = (platform.root / "worker.log").read_text(errors="replace")
        refused = log.count("published TAP topology or policy has changed") >= 2
        return health if scope and journal == [("unknown",), ("unknown",)] and refused else None

    try:
        blocked = wait_for(fenced, timeout=30, retry=(OSError, RuntimeError))
        request_fault("E_RECOVERING", lambda: worker(platform.worker, "CompleteRecovery", {
            "broker_session_id": blocked["broker_session_id"]}))
        request_fault("E_RECOVERING", lambda: connect_service(platform, native[1], attached[0]))
        in_router(pin, "nft", "list", "table", "ip", bad_table, stdout=subprocess.DEVNULL)
        print("Changed private ingress policy kept both TAPs unknown and new dials fenced", flush=True)
    finally:
        in_router(pin, "nft", "delete", "table", "ip", bad_table)

    final = ready_worker(platform, binary, recovered)
    assert final["broker_session_id"] == blocked["broker_session_id"]
    restored = current()
    assert_original_ports(attached, restored)
    dns_pair()
    assert_business(before)

    for index, (machine, _, _) in enumerate(guests):
        path = f"machines/{machine}"
        view = platform.wait_capability(path, "lifecycle.stop")
        platform.complete(platform.change(path + "/stop", "a20-release-" + machine, {}, view))
        if index == 0:
            assert list(pin_root.iterdir()) == [pin]
            receipt = business_request(platform, fixed[1], restored[1], "remaining")
            assert platform.guest(fixed[1], ["tail", "-n", "1", "/run/a20-business"]) == receipt.encode() + b"\n"
    assert list(pin_root.iterdir()) == []
    assert read_journal(platform, "SELECT state FROM network_tap_leases") == [("retired",), ("retired",)]
    print("First Stop retained the other route. Final Stop retired both TAPs and unmounted the router pin.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires Linux KVM and root on a systemd host")
    if os.environ.get("WORKER_NETWORK_RECOVERY_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "WORKER_NETWORK_RECOVERY_NAMESPACE": "1",
                    "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a20-network-") as temp, ExitStack() as stack:
        root = Path(temp)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=7G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "-l", root], check=True)
        slice_name = f"smolvmnetworkrecovery{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        network = dict(network_id="stream-net", route_domain_id="a20-recovery", cidr="10.32.71.0/24")
        platform = Platform(args, root, stack, (network,), use_installation=True)
        try:
            run(platform)
        except BaseException:
            for log in (root / "platform.log", root / "serve.log", root / "worker.log"):
                if log.exists():
                    print(log.name, log.read_text(errors="replace")[-4000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
