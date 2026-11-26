#!/usr/bin/env python3
"""Exercise managed Worker restart on real Linux/KVM from an isolated Bundle.

Use durable-reader with the original installed Bundle and --replacement-worker.
Use legacy-reader-no-reexport for a Bundle whose VMM lacks CAPTURE_RECOVER_FDS.
Use cpu-limit-restart with a Bundle whose Worker supports managed CPU limits
and whose Serve implements the descriptor recovery protocol. The CPU run also
uses the public settings and hibernate APIs.
"""

import argparse
from contextlib import ExitStack, suppress
import grp
import hashlib
import json
import os
from pathlib import Path
import select
import signal
import socket
import sqlite3
import struct
import sys
import tempfile

from managed_system_vsock import Platform, cpu_limit_case, digest, wait_for, worker


def launch_system(platform):
    platform.start()
    result = platform.change("machines", "create-worker-restart-fixture", {
        "name": "Worker restart fixture",
        "template_id": "t_system",
        "boot_mode": "system",
        "runtime_instance_id": platform.serve_id,
        "start": True,
        "resources": {"cpu_count": 2, "memory_mib": 768},
    })
    platform.complete(result)
    machine_id = result["machine_id"]
    _, execution = platform.context(machine_id)
    wait_for(lambda: platform.guest(execution, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    helper = platform.guest(execution, ["/usr/bin/sha256sum", "/sbin/smolvm-identity-helper"]).split()[0]
    assert "sha256:" + helper.decode() == digest(platform.bundle / "guest/identity-helper")
    return machine_id, execution


def worker_scope(platform, execution, bundle=None):
    return {"route_id": "deep", "bundle_digest": bundle or platform.bundle_id, "expected": execution}


def cpu_operation(platform, execution, operation_id, payload):
    body = json.dumps([operation_id, execution, payload], sort_keys=True).encode()
    return {"operation_id": operation_id, "request_digest": "sha256:" + hashlib.sha256(body).hexdigest(),
            **worker_scope(platform, execution)}


def request_fault(code, callback):
    try:
        callback()
    except RuntimeError as error:
        assert error.args[0]["code"] == code, error
    else:
        raise AssertionError(f"expected Worker fault {code}")


def vmm_pid(platform, execution):
    instance = execution["instance_id"]
    return platform.native(f"/instances/{instance}/inventory")["execution"]["pid"]


def restart_worker(platform, binary):
    pid_struct = struct.Struct("3i")
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.connect(str(platform.worker))
        pid, uid, _ = pid_struct.unpack(connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, pid_struct.size))
    assert uid == 0
    fd = os.pidfd_open(pid)
    try:
        poll = select.poll()
        poll.register(fd, select.POLLIN)
        signal.pidfd_send_signal(fd, signal.SIGKILL)
        assert poll.poll(10_000), "original Worker did not exit"
    finally:
        os.close(fd)
    platform.spawn([binary, "serve", "--socket", platform.worker,
                    "--state-dir", platform.root / "catalog",
                    "--socket-gid", str(grp.getgrnam("kvm").gr_gid)], "worker.log")


def ready_worker(platform, binary, previous_health):
    def ready():
        info = worker(platform.worker, "Health")
        return info if info["state"] == "ready" else None

    info = wait_for(ready, timeout=65, retry=(OSError, RuntimeError))
    assert info["broker_session_id"] != previous_health["broker_session_id"]
    assert info["executable_sha256"] == digest(binary)
    return info


def cpu_restart(platform, machine_id, original):
    machine = "machines/" + machine_id
    cpu_limit_case(platform, machine, machine_id, original, historical=False)
    _, execution = platform.context(machine_id)
    budget = worker(platform.worker, "QueryWorkerBudget")["allocated"]["vcpus"]
    assert budget == 2

    def query():
        return worker(platform.worker, "QueryManagedCPULimit", **worker_scope(platform, execution))

    def kernel_readback():
        pid = vmm_pid(platform, execution)
        cgroups = Path(f"/proc/{pid}/cgroup").read_text().splitlines()
        group, = [line.removeprefix("0::/") for line in cgroups if line.startswith("0::/")]
        quota, period = (Path("/sys/fs/cgroup") / group / "cpu.max").read_text().split()
        return pid, int(quota), int(period)

    default = query()
    assert default["requested_millicpus"] is None and default["quota_usec"] == budget * default["period_usec"]
    request_fault("E_STALE_EXECUTION", lambda: worker(
        platform.worker, "QueryManagedCPULimit", **worker_scope(platform, original)))
    unknown_bundle = "sha256:" + "ab" * 32
    foreign = worker_scope(platform, execution, unknown_bundle)
    request_fault("E_BUNDLE_UNAVAILABLE", lambda: worker(platform.worker, "QueryManagedCPULimit", **foreign))
    worker(platform.worker, "RegisterBundle", {"executable_sha256": digest(platform.bundle / "smolvm-bin")},
           bundle_digest=unknown_bundle)
    request_fault("E_NOT_FOUND", lambda: worker(platform.worker, "QueryManagedCPULimit", **foreign))
    for invalid in ({}, {"millicpus": 0}, {"millicpus": -1}, {"millicpus": 1.25}, {"millicpus": 2001}):
        request_fault("E_INVALID_REQUEST", lambda: worker(
            platform.worker, "SetManagedCPULimit", invalid,
            **cpu_operation(platform, execution, "reject-invalid-quota", invalid)))

    payload = {"millicpus": 3}
    replay = cpu_operation(platform, execution, "replay-lost-quota-receipt", payload)
    blocker = cpu_operation(platform, execution, "block-parallel-quota", {})
    worker(platform.worker, "BeginOperation", {"kind": "catalog"}, **blocker)
    try:
        request_fault("E_OPERATION_IN_PROGRESS", lambda: worker(
            platform.worker, "SetManagedCPULimit", payload, **replay))
        assert query() == default
    finally:
        worker(platform.worker, "FinishOperation", {"abort": True, "expected_revision": 1}, **blocker)

    frame = {"protocol": "WorkerResource/1", "request_id": "lost-cpu-quota-reply",
             "method": "SetManagedCPULimit", "payload": payload, **replay}
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.connect(str(platform.worker))
        connection.sendall(json.dumps(frame).encode())

    def quota_applied():
        readback = query()
        return readback if readback["requested_millicpus"] == 3 else None

    actual = wait_for(quota_applied, timeout=20)
    pid, quota, period = kernel_readback()
    assert (quota, period) == (actual["quota_usec"], actual["period_usec"]) == (1002, 334000)
    request_fault("E_IDEMPOTENCY_CONFLICT", lambda: worker(
        platform.worker, "SetManagedCPULimit", {"millicpus": 4}, **replay))
    request_fault("E_IDEMPOTENCY_CONFLICT", lambda: worker(
        platform.worker, "SetManagedCPULimit", payload, **{**replay, **worker_scope(platform, original)}))

    binary = platform.bundle / "smolvm-resourced"
    previous = worker(platform.worker, "Health")
    restart_worker(platform, binary)
    ready_worker(platform, binary, previous)
    assert worker(platform.worker, "SetManagedCPULimit", payload, **replay) == actual
    assert query() == actual and kernel_readback() == (pid, quota, period)
    assert worker(platform.worker, "QueryWorkerBudget")["allocated"]["vcpus"] == budget

    clear = {"millicpus": None}
    assert worker(platform.worker, "SetManagedCPULimit", clear,
                  **cpu_operation(platform, execution, "clear-test-quota", clear)) == default
    print(json.dumps({"case": "cpu-limit-restart", "bundle": platform.bundle_id,
                      "pid": pid, "replayed_after_worker_restart": actual, "reserved_vcpus": budget},
                     sort_keys=True), flush=True)


def durable_lease(database, execution):
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as reader:
        schema = reader.execute("PRAGMA user_version").fetchone()[0]
        rows = reader.execute("""
            SELECT f.state, f.delivered, f.revision, f.lease_id
            FROM fd_leases f JOIN statepoint_restore_leases s USING(lease_id)
            WHERE f.incarnation_id=?""", (execution["incarnation_id"],)).fetchall()
    assert len(rows) == 1, rows
    return schema, rows[0]


def retained_anon_ram(pid):
    identities = set()
    for entry in Path(f"/proc/{pid}/fd").iterdir():
        with suppress(FileNotFoundError):
            if str(entry.readlink()).endswith("/materialized/memory.bin (deleted)"):
                stat = entry.stat()
                identities.add((stat.st_dev, stat.st_ino, stat.st_size))
    assert len(identities) == 1, identities
    return next(iter(identities))


def durable_reader_restart(platform, machine_id, old_execution, replacement, expect_legacy):
    machine = "machines/" + machine_id
    guest_marker = "/var/tmp/worker-restart-marker"
    platform.guest(old_execution, ["/bin/sh", "-c", f"printf original-reader > {guest_marker}; sync"])
    platform.complete(platform.change(machine + "/hibernate", "save-reader-fixture", {}, platform.app(machine)))
    platform.complete(platform.change(machine + "/start", "restore-reader-fixture", {}, platform.app(machine)))
    _, execution = platform.context(machine_id)
    assert execution["execution_epoch"] == old_execution["execution_epoch"] + 1
    assert execution["incarnation_id"] != old_execution["incarnation_id"]
    pid = vmm_pid(platform, execution)
    assert platform.guest(execution, ["/bin/cat", guest_marker]) == b"original-reader"
    backing = retained_anon_ram(pid)
    database = platform.root / "catalog/catalog.sqlite"
    initial_schema, before = durable_lease(database, execution)
    assert before[:3] == ("attached", 2, 2), before

    previous = worker(platform.worker, "Health")
    restart_worker(platform, replacement)
    if expect_legacy:
        def observed_unsupported():
            with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as reader:
                unconfirmed_writer = reader.execute(
                    "SELECT count(*) FROM writer_grants WHERE incarnation_id=? AND state='unknown'",
                    (execution["incarnation_id"],)).fetchone()[0]
            log = (platform.root / "worker.log").read_text(errors="replace")
            _, lease = durable_lease(database, execution)
            return (unconfirmed_writer and lease == ("unknown", 2, 3, before[3])
                    and "ERR EINVAL unknown command" in log)

        wait_for(observed_unsupported, timeout=20, retry=(OSError, RuntimeError, sqlite3.Error))
        health = worker(platform.worker, "Health")
        assert health["state"] == "recovering" and health["executable_sha256"] == digest(replacement)
    else:
        health = ready_worker(platform, replacement, previous)
        assert durable_lease(database, execution)[1] == ("attached", 2, 4, before[3])
        assert retained_anon_ram(pid) == backing
        assert platform.guest(execution, ["/bin/cat", guest_marker]) == b"original-reader"
        assert platform.context(machine_id)[1] == execution

    assert vmm_pid(platform, execution) == pid
    final_schema, after = durable_lease(database, execution)
    assert final_schema >= initial_schema
    print(json.dumps({
        "case": "legacy-reader-no-reexport" if expect_legacy else "durable-reader",
        "bundle": platform.bundle_id, "pid": pid, "retained_ram_inode": backing,
        "worker_health": health["state"], "reader_before": before, "reader_after": after,
        "schema_before": initial_schema, "schema_after": final_schema,
        "worker_before": previous["executable_sha256"], "worker_after": health["executable_sha256"],
    }, sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", required=True,
                        choices=("cpu-limit-restart", "durable-reader", "legacy-reader-no-reexport"))
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, required=True, type=lambda p: Path(p).resolve(strict=True))
    parser.add_argument("--replacement-worker", type=lambda p: Path(p).resolve(strict=True))
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root with a live systemd manager and /dev/kvm")
    if args.case != "cpu-limit-restart" and args.replacement_worker is None:
        parser.error("--replacement-worker is required for a durable reader case")
    if os.environ.get("SMOLVM_WORKER_RESTART_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "SMOLVM_WORKER_RESTART_KVM_NAMESPACE": "1"})

    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="worker-kvm-") as temporary, ExitStack() as stack:
        root = Path(temporary)
        root.chmod(0o755)
        platform = Platform(args, root, stack)
        try:
            machine_id, execution = launch_system(platform)
            if args.case == "cpu-limit-restart":
                cpu_restart(platform, machine_id, execution)
            else:
                durable_reader_restart(platform, machine_id, execution, args.replacement_worker,
                                       args.case == "legacy-reader-no-reexport")
        except BaseException:
            for log in (root / "worker.log", root / "serve.log", root / "platform.log"):
                if log.is_file():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-6000:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
