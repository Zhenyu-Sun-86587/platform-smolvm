#!/usr/bin/env python3
"""Prove Worker GC over a live Debian checkpoint, save, export and operation."""

import argparse
import array
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
from threading import Event

from managed_system_vsock import Platform, digest, wait_for, worker
from worker_restart_kvm import cpu_operation as scoped_operation, request_fault, restart_worker


class GCPlatform(Platform):
    def spawn(self, command, log):
        if Path(command[0]).name != "smolvm-resourced":
            return super().spawn(command, log)
        standard = self.env
        self.env = {**standard, "LD_PRELOAD": str(self.args.cut_library),
                    "FIXTURE_GC_TARGET": str(self.root / "cut-target"),
                    "FIXTURE_GC_REACHED": str(self.root / "cut-reached")}
        try:
            return super().spawn(command, log)
        finally:
            self.env = standard


def db_rows(db, sql, *parameters):
    with sqlite3.connect(db.as_uri() + "?mode=ro", uri=True) as connection:
        return connection.execute(sql, parameters).fetchall()


def backup_file(platform, method, payload, role):
    packet = dict(protocol="WorkerResource/1", request_id=method, method=method, payload=payload,
                  route_id="deep", bundle_digest=platform.bundle_id)
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.settimeout(120)
        connection.connect(str(platform.worker))
        connection.sendall(json.dumps(packet).encode())
        data, controls, flags, _ = connection.recvmsg(65536, socket.CMSG_SPACE(array.array("i").itemsize),
                                                     socket.MSG_CMSG_CLOEXEC)
    received = array.array("i")
    for level, kind, raw in controls:
        if (level, kind) == (socket.SOL_SOCKET, socket.SCM_RIGHTS):
            received.frombytes(raw)
    try:
        assert not flags & (socket.MSG_TRUNC | socket.MSG_CTRUNC), flags
        reply = json.loads(data)
        if "error" in reply:
            raise RuntimeError(reply["error"])
        assert len(received) == 1 and reply["fd_table"] == [
            {"index": 0, "resource_id": payload["backup_id"], "role": role}
        ], reply
        return reply["result"], os.fdopen(received.pop(), "rb")
    finally:
        for fd in received:
            os.close(fd)


def run(platform):
    platform.start()
    runtime_root = platform.root / "catalog"
    db = runtime_root / "catalog.sqlite"
    binary = platform.bundle / "smolvm-resourced"
    manifest = json.loads((platform.bundle / "runtime-bundle.json").read_text())
    print("Bundle", platform.bundle_id, "reader", manifest["contracts"]["state_reader_id"],
          "Worker", digest(binary), "platformd", digest(platform.args.platformd), flush=True)
    accepted = platform.change("machines", "a05-create", {
        "name": "GC proof", "template_id": "t_system", "boot_mode": "system",
        "runtime_instance_id": platform.serve_id, "start": False,
        "resources": {"cpu_count": 1, "memory_mib": 512},
    })
    platform.complete(accepted)
    machine_id = accepted["machine_id"]
    machine = "machines/" + machine_id
    platform.complete(platform.change(machine + "/start", "a05-cold-start", {}, platform.app(machine)))
    _, ref = platform.context(machine_id)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    marker = os.urandom(20).hex()
    platform.guest(ref, ["/bin/sh", "-c", f"echo {marker} >/run/a05-ram; echo {marker} >/root/a05-disk; sync"])

    def call(method, payload, **scope):
        route = {"route_id": "deep", "bundle_digest": platform.bundle_id, **scope}
        return worker(platform.worker, method, payload, **route)

    def resource(identifier):
        return call("QueryResource", {"resource_id": identifier})

    def closure(kind, owner):
        reply = call("QueryOwnerClosure", {"owner_kind": kind, "owner_id": owner})
        return {row["resource_id"]: row["state"] for row in reply["resources"]}

    def release_point(point):
        return call("ReleaseStatePoint", {"capture_id": point["capture_id"],
                                          "manifest_digest": point["manifest_digest"]})

    def prepare_backup(key, captures, generations=()):
        result, file = backup_file(platform, "PrepareBackupClosure", {
            "backup_id": key, "state_reader_id": manifest["contracts"]["state_reader_id"],
            "capture_ids": list(captures), "generation_ids": list(generations), "bundle_manifest": manifest,
        }, "backup_manifest")
        assert result["backup_id"] == key, result
        with file:
            return json.load(file)

    saved = platform.complete(platform.change(machine + "/snapshots", "a05-checkpoint",
                                               {"capture_mode": "vm"}, platform.app(machine)))["result"]
    point = saved["statepoint"]
    point_id = point["representation_id"]
    target, = [row[0] for row in db_rows(db, "SELECT child_resource_id FROM resource_edges WHERE owner_resource_id=?", point_id)]
    target_file = runtime_root / "objects" / target / "data"
    target_stat = target_file.stat()
    assert target_stat.st_blocks > 0, target_stat
    target_digest = digest(target_file)
    revisions = {target: resource(target)["revision"], point_id: resource(point_id)["revision"]}
    sleeping = platform.complete(platform.change(machine + "/hibernate", "a05-sleep", {},
                                                   platform.app(machine)))["result"]["statepoint"]
    sleeping_id = sleeping["representation_id"]
    revisions[sleeping_id] = resource(sleeping_id)["revision"]
    points = runtime_root / "statepoints"
    checkpoint_chunks = {p.name for p in (points / point_id / "objects").iterdir() if len(p.name) == 64}
    hibernate_chunks = {p.name for p in (points / sleeping_id / "objects").iterdir() if len(p.name) == 64}
    exclusive_chunks = checkpoint_chunks - hibernate_chunks
    shared_chunks = checkpoint_chunks & hibernate_chunks
    assert exclusive_chunks and shared_chunks
    assert platform.app(machine)["power_state"] == "hibernated"
    assert {target, point_id} <= closure("statepoint_checkpoint", point["capture_id"]).keys()
    assert {target, sleeping_id} <= closure("statepoint_hibernate", sleeping["capture_id"]).keys()
    request_fault("E_RESOURCE_PINNED", lambda: release_point(sleeping))
    print("Checkpoint", point_id, "Hibernate", sleeping_id, "shared block", target,
          "inode", target_stat.st_ino, "allocated_bytes", target_stat.st_blocks * 512,
          "digest", target_digest, flush=True)

    op_state = dict(instance_id="a05-dependent-transaction", incarnation_id=None, execution_epoch=0)
    operation = scoped_operation(platform, op_state, "a05-unfinished", {"resource_id": target})
    assert call("BeginOperation", {"kind": "catalog"}, **operation)["phase"] == "NEW"
    root = dict(owner_kind="operation", owner_id=operation["operation_id"], resource_id=target)
    call("AddOwnerRef", {**root, "expected_revision": 0}, **operation)
    initial_backup = "a05-captured-export"
    backup = prepare_backup(initial_backup, [point["capture_id"], sleeping["capture_id"]])
    assert backup["block_digests"][target] == target_digest
    assert {target, point_id, sleeping_id} <= closure("backup", initial_backup).keys()
    assert target in closure("operation", operation["operation_id"])

    def reject_gc(identifier):
        request_fault("E_RESOURCE_PINNED", lambda: call("MarkResourceForGC", {
            "resource_id": identifier, "expected_revision": revisions[identifier],
        }))

    stop = Event()

    def compete():
        conflicts = 0
        while not stop.is_set():
            for identifier in revisions:
                reject_gc(identifier)
                conflicts += 1
            stop.wait(0.02)
        return conflicts

    with ThreadPoolExecutor(max_workers=2) as concurrent:
        pressure = concurrent.submit(compete)
        try:
            snapshot_url = "snapshots/" + saved["snapshot_id"]
            revision = platform.app(snapshot_url)["revision"]
            headers = {"If-Control-Epoch": platform.epoch, "Idempotency-Key": "a05-delete-checkpoint",
                       "If-Match": f'"{revision}"'}
            deleted = platform.app(snapshot_url, "DELETE", {}, headers, status=202)
            platform.complete(deleted)
            replay = platform.app(snapshot_url, "DELETE", {}, headers, status=202)
            assert replay["operation_id"] == deleted["operation_id"]
            assert platform.app(snapshot_url)["state"] == "deleted"
            assert not closure("statepoint_checkpoint", point["capture_id"])
            assert resource(point_id)["state"] == "available"
            request_fault("E_RESOURCE_UNAVAILABLE", lambda: prepare_backup("a05-stale-cut", [point["capture_id"]]))
            result, file = backup_file(platform, "ReadBackupMember", {
                "backup_id": initial_backup, "member": f"blocks/{target}/data",
            }, "backup_member")
            with file:
                assert os.fstat(file.fileno()).st_ino == target_stat.st_ino
                assert result["bytes"] == target_stat.st_size
            assert target_file.stat().st_ino == target_stat.st_ino
        finally:
            stop.set()
        conflicts = pressure.result(timeout=10)
    assert conflicts >= 3, conflicts
    print("Concurrent fenced GC attempts", conflicts,
          "public Delete replay", deleted["operation_id"], "backup retained the same disk inode", flush=True)

    assert call("ReleaseBackupClosure", {"backup_id": initial_backup})["phase"] == "released"
    assert not closure("backup", initial_backup)
    wait_for(lambda: resource(point_id)["state"] == "deleted")
    assert not (points / point_id).exists()
    chunk_cache = points / ".chunks" / "objects"
    assert all(not (chunk_cache / name).exists() for name in exclusive_chunks)
    assert all((chunk_cache / name).stat().st_nlink >= 2 for name in shared_chunks)
    print("Checkpoint collected", len(exclusive_chunks), "private chunks and retained", len(shared_chunks),
          "chunks still referenced by the hibernation", flush=True)
    reject_gc(target)
    reject_gc(sleeping_id)
    assert digest(target_file) == target_digest
    after = platform.complete(platform.change(machine + "/start", "a05-exact-wake", {}, platform.app(machine)))
    _, newer = platform.context(machine_id)
    assert newer["execution_epoch"] == ref["execution_epoch"] + 1, after
    for name in ("/run/a05-ram", "/root/a05-disk"):
        assert platform.guest(newer, ["/bin/cat", name]).strip().decode() == marker
    print("Hibernate still restored the saved Guest RAM and root-disk marker", marker,
          "epoch", newer["execution_epoch"], flush=True)
    platform.wait_capability(machine, "lifecycle.stop")
    platform.complete(platform.change(machine + "/stop", "a05-stop", {}, platform.app(machine)))
    platform.complete(platform.change(machine, "a05-delete-machine", {}, platform.app(machine), method="DELETE"))
    assert release_point(sleeping)["phase"] == "released"
    wait_for(lambda: resource(sleeping_id)["state"] == "deleted")
    assert not (points / sleeping_id).exists()
    assert all(not (chunk_cache / name).exists() for name in hibernate_chunks)

    # A pending operation and a subsequent independent block export each keep
    # the same object. Finish the operation before releasing the final export.
    wait_for(lambda: not db_rows(db, "SELECT 1 FROM resource_edges WHERE child_resource_id=?", target))
    assert db_rows(db, "SELECT owner_kind,owner_id FROM root_refs WHERE resource_id=? AND state='active'", target) == [
        ("operation", operation["operation_id"])
    ]
    reject_gc(target)
    final_export = "a05-block-export"
    assert prepare_backup(final_export, [], [target])["block_digests"][target] == target_digest
    call("DropOwnerRef", {**root, "expected_revision": 1}, **operation)
    assert call("FinishOperation", {"abort": False, "expected_revision": 1}, **operation)["phase"] == "COMPLETED"
    assert db_rows(db, "SELECT owner_kind,owner_id FROM root_refs WHERE resource_id=? AND state='active'", target) == [
        ("backup", final_export)
    ]
    reject_gc(target)
    assert digest(target_file) == target_digest
    print("Independently refused collection with operation alone and block export alone", flush=True)

    prior = call("Health", {})
    (platform.root / "cut-target").write_text(str(target_file))
    assert call("ReleaseBackupClosure", {"backup_id": final_export})["phase"] == "released"
    assert wait_for((platform.root / "cut-reached").exists, timeout=20)
    tombstone, = db_rows(db, "SELECT state,revision FROM resources WHERE resource_id=?", target)
    assert tombstone == ("deleting", revisions[target] + 1), tombstone
    assert target_file.stat().st_ino == target_stat.st_ino and digest(target_file) == target_digest
    assert db_rows(db, "SELECT child_resource_id FROM resource_edges WHERE owner_resource_id=?", target)
    print("Stopped before unlink", tombstone, "file and outgoing edge retained", flush=True)
    (platform.root / "cut-target").unlink()
    restart_worker(platform, binary)
    recovered = wait_for(lambda: call("Health", {}), retry=(OSError, RuntimeError))
    assert recovered["state"] == "recovering" and recovered["broker_session_id"] != prior["broker_session_id"]
    assert recovered["executable_sha256"] == prior["executable_sha256"] == digest(binary)
    request_fault("E_RECOVERING", lambda: call("CollectResource", {
        "resource_id": target, "expected_revision": tombstone[1],
    }))
    assert target_file.stat().st_ino == target_stat.st_ino
    assert call("CompleteRecovery", {"broker_session_id": recovered["broker_session_id"]})["state"] == "ready"
    deleted_row = wait_for(lambda: (current := resource(target))["state"] == "deleted" and current)
    assert deleted_row["revision"] == tombstone[1] + 1 and not target_file.parent.exists(), deleted_row
    assert call("CollectResource", {"resource_id": target, "expected_revision": tombstone[1]}) == deleted_row
    assert not db_rows(db, "SELECT 1 FROM resource_edges WHERE owner_resource_id=? OR child_resource_id=?", target, target)
    assert not db_rows(db, "SELECT 1 FROM root_refs WHERE resource_id=? AND state='active'", target)
    assert digest(runtime_root / "objects/fixture-system-root/data") == digest(platform.args.system_image)
    print("Worker", prior["broker_session_id"], "→", recovered["broker_session_id"],
          "replayed tombstone automatically; inode removed; final revision", deleted_row["revision"],
          "no live references; template bytes retained", flush=True)

    config = platform.root / "platform.json"
    settings = json.loads(config.read_text())
    settings["runtime"]["custom"] = []
    config.write_text(json.dumps(settings))
    platform.restart()
    replay = platform.app(snapshot_url, "DELETE", {}, headers, status=202)
    assert replay["operation_id"] == deleted["operation_id"] and replay["state"] == "succeeded", replay
    conflict = platform.app(snapshot_url, "DELETE", {}, {**headers, "If-Match": f'"{revision + 1}"'}, status=409)
    assert conflict["error"]["code"] == "IDEMPOTENCY_CONFLICT", conflict
    fresh = platform.app(snapshot_url, "DELETE", {}, {**headers, "Idempotency-Key": "a05-new-delete"}, status=409)
    assert fresh["error"]["code"] == "NOT_SUPPORTED", fresh
    print("Retired the runtime route and restarted platformd; the original DELETE returned its completed",
          "receipt, the same key rejected a changed revision and a fresh DELETE required the original route", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ("bundle", "platformd", "system-image", "cut-library"):
        parser.add_argument("--" + arg, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, Linux KVM, and a live systemd manager")
    if os.environ.get("WORKER_GC_FIXTURE_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "WORKER_GC_FIXTURE_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a05-") as base, ExitStack() as stack:
        root = Path(base)
        root.chmod(0o755)
        name = f"smolvmgc{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        name, "fail", "1", "MemoryAccounting", "b", "true", "0"], check=True,
                       stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = name
        platform = GCPlatform(args, root, stack, state_tmpfs=True)
        try:
            run(platform)
        except BaseException:
            for file in (root / "platform.log", root / "worker.log", root / "serve.log",
                         *(root / "state/managed").glob("*.log")):
                if file.exists():
                    print(file.name, file.read_text(errors="replace")[-6000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
