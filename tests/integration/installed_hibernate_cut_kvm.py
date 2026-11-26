#!/usr/bin/env python3
"""Measure the installed Hibernate cut with and without a live volatile child."""

import argparse
from contextlib import closing, suppress
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time

from installed_fork_backing_kvm import (capture_catalog, execution_inventory, guest_lines, host_page_address,
                                        invoke_fork, page_residency, peer, proc_maps)
from installed_fork_lineage_kvm import attached_volumes, backing_owners, execution, native


def sha256(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def ticks(root, ref):
    return [line.split()[1:] for line in guest_lines(root, ref, "PARKED_BLOCK_TICK ")
            if len(line.split()) == 6]


def lookup(database, sql, values):
    with closing(sqlite3.connect(f"file:{database}?mode=ro", uri=True)) as reader:
        return reader.execute(sql, values).fetchone()


def export_cut(database, capture):
    deadline = time.monotonic() + 25
    while time.monotonic() < deadline:
        row = lookup(database, """
            SELECT p.phase,p.volatile_id,p.representation_id,p.point_json,w.plan_json,o.revision
            FROM statepoints p JOIN operations o USING(operation_id) JOIN writer_pivots w USING(operation_id)
            WHERE p.capture_id=?
        """, (capture,))
        if row:
            phase, volatile, representation, point, pivot, revision = row
            assert phase in ("exporting", "durable"), row[:3]
            return (phase, volatile, representation, json.loads(point), json.loads(pivot)["heads"], revision)
        time.sleep(.004)
    raise TimeoutError(f"missed export of {capture}")


def retained_pins(database, resource):
    return lookup(database, """
        SELECT state,(SELECT count(*) FROM root_refs WHERE resource_id=:resource AND state='active'),
                     (SELECT count(*) FROM fd_leases WHERE resource_id=:resource AND state!='released')
        FROM resources WHERE resource_id=:resource
    """, {"resource": resource})


def disk_fingerprints(root, heads):
    return {item[key]: sha256(root / "catalog/objects" / item[key] / "data")
            for item in heads for key in ("head_resource_id", "next_resource_id")}


def ram_backings(pids):
    backings = {}
    for pid in pids:
        with suppress(FileNotFoundError):
            for descriptor in Path(f"/proc/{pid}/fd").iterdir():
                with suppress(FileNotFoundError):
                    if not os.readlink(descriptor).startswith("/memfd:smolvm-guest-ram "):
                        continue
                    meta = descriptor.stat()
                    assert meta.st_nlink == 0
                    backings[meta.st_dev, meta.st_ino, meta.st_size] = meta.st_blocks * 512
    return backings


def measure_release(before, survivors):
    rows = []
    for identity, allocated in sorted(before.items()):
        owners = backing_owners(survivors, {identity})
        rows.append({"device": identity[0], "inode": identity[1], "length": identity[2],
                     "allocated_bytes": allocated, "owners_after": owners})
    released = sum(row["allocated_bytes"] for row in rows if not row["owners_after"])
    retained = sum(row["allocated_bytes"] for row in rows if row["owners_after"])
    return rows, released, retained


def submit(launch, endpoint, kind, operation_id, ref, parameters):
    body = {"kind": kind, "operation_id": operation_id, "request_digest": launch.digest(operation_id.encode()),
            "expected": execution(ref), "parameters": parameters}
    accepted = launch.http(endpoint, launch.EXT + "/operations", body)
    assert accepted["phase"] == "NEW", accepted
    return body


def cancel(launch, endpoint, body, revision):
    return launch.http(endpoint, f"{launch.EXT}/operations/{body['operation_id']}/cancel",
                       {"request_digest": body["request_digest"], "expected": body["expected"],
                        "expected_revision": revision})


def verify(launch, endpoint, root, source, with_child, guest_sha):
    caps = launch.http(endpoint, launch.EXT + "/capabilities")
    worker = root / "ipc/worker.sock"
    worker_pid = peer(worker, socket.SOCK_SEQPACKET)
    serve_pid = peer(endpoint, socket.SOCK_STREAM)
    database = root / "catalog/catalog.sqlite"
    machine = "test-managed"
    origin = execution_inventory(launch, endpoint, source)
    original_volumes = set(attached_volumes(launch, endpoint, source).values())
    source_pid = origin["pid"]

    child = child_pid = cold_addresses = cold_before = branch_id = child_lease = None
    child_backings = set()
    if with_child:
        batch = invoke_fork(launch, endpoint, source, "a07-fork-active-child", ["a07-child"])
        child, = batch["children"]
        child_pid = execution_inventory(launch, endpoint, child)["pid"]
        branch_id = batch["capture_id"]
        descriptor, identities, leases = capture_catalog(database, branch_id, [child])
        child_backings = set(identities.values())
        child_lease = leases[child["incarnation_id"]]
        armed, = launch.until(lambda: guest_lines(root, child, "A07_CHILD_WAIT "), 15)
        assert armed.split()[1] == ticks(root, source)[-1][1]
        addresses, = guest_lines(root, source, "A07_GPAS ")
        gpas = [int(value) for value in addresses.split()[1:]]
        child_maps = proc_maps(child_pid)
        sampled = {gpa: host_page_address(descriptor, identities, child_maps, gpa) for gpa in gpas}
        cold_before = page_residency(child_pid, sampled)
        cold_addresses = {gpa: sampled[gpa] for gpa, page in cold_before.items() if not page["present"]}
        assert len(cold_before) == 6 and cold_addresses, cold_before

    parked = native(launch, endpoint, "standby", "a07-standby-before-error", source, {"mode": "managed"})
    assert parked["native"]["source_disposition"] == "STANDBY"
    standby_ticks = ticks(root, source)
    capture = "a07-aborted-point"
    chunks = root / "catalog/statepoints/.chunks"
    with chunks.open("xb") as obstruction:
        obstruction.write(b"private scratch export fault")
    try:
        request = submit(launch, endpoint, "hibernate", "a07-abort-export", source, {"capture_id": capture})
        phase, aborted_ram, _, _, _, revision = export_cut(database, capture)
        failed, = launch.until(lambda: lookup(database, """
            SELECT error FROM statepoints WHERE capture_id=? AND phase='exporting'
                AND manifest_digest IS NULL AND error IS NOT NULL
        """, (capture,)), 25)
        assert "(os error 17)" in failed or "(os error 20)" in failed, failed
        accepted = cancel(launch, endpoint, request, revision)
        assert accepted["phase"] == "NEW", accepted
        rolled_back = launch.settled(endpoint, request["operation_id"])
        assert rolled_back["phase"] == "ABORTED", rolled_back
    finally:
        chunks.unlink()
    assert lookup(database, "SELECT phase FROM statepoints WHERE capture_id=?", (capture,)) == ("aborted",)
    assert retained_pins(database, aborted_ram)[1:] == (0, 0)
    same = execution_inventory(launch, endpoint, source)
    assert all(same[key] == origin[key] for key in ("pid", "pid_start_time", "incarnation_id", "execution_epoch"))
    assert rolled_back["statepoint"]["original_source_disposition"] == "STANDBY"
    assert lookup(database, "SELECT operation_id FROM native_standby WHERE incarnation_id=?",
                  (source["incarnation_id"],)) == ("a07-standby-before-error",)
    assert ticks(root, source) == standby_ticks
    wake = native(launch, endpoint, "wake", "a07-explicit-wake", source, {})
    def resumed_ticks():
        rows = ticks(root, source)
        return rows if len(rows) > len(standby_ticks) else None

    continued = launch.until(resumed_ticks, 15)
    assert wake["native"]["source_disposition"] == "RUNNING"
    assert continued[len(standby_ticks)][:2] == [str(int(standby_ticks[-1][0]) + 1), standby_ticks[-1][1]]

    parked = native(launch, endpoint, "standby", "a07-standby-to-save", source, {"mode": "managed"})
    assert parked["native"]["source_disposition"] == "STANDBY"
    frozen_ticks = ticks(root, source)
    child_before = ticks(root, child)[-1] if with_child else None
    holders = {source_pid, worker_pid, serve_pid}
    ram_before = ram_backings(holders)
    assert ram_before and child_backings <= ram_before.keys()
    started = time.monotonic_ns()
    capture = "a07-durable-hibernate"
    with os.fdopen(os.pidfd_open(source_pid), "rb", buffering=0) as pidfd:
        request = submit(launch, endpoint, "hibernate", "a07-hibernate", source, {"capture_id": capture})
        exporting, volatile_id, representation, point, heads, _ = export_cut(database, capture)
        assert len(heads) == 2 and ticks(root, source) == frozen_ticks
        cut_pins = retained_pins(database, volatile_id)
        assert cut_pins[0] == "available" and cut_pins[1] >= 1, cut_pins
        ram_before.update(ram_backings(holders))
        cut_disk = disk_fingerprints(root, heads)
        saved = launch.settled(endpoint, request["operation_id"], 180)
        assert saved["phase"] == "COMPLETED", saved
        assert select.select([pidfd], [], [], 10)[0]
    elapsed = (time.monotonic_ns() - started) / 1e6
    saved_point = saved["statepoint"]
    assert saved_point["phase"] == "hibernated" and saved_point["source_released"]
    assert saved_point["representation_id"] == representation and saved_point["capture_id"] == capture
    assert ticks(root, source) == frozen_ticks and disk_fingerprints(root, heads) == cut_disk
    assert retained_pins(database, volatile_id)[1:] == (0, 0)
    binding = launch.http(endpoint, f"{launch.EXT}/machines/{machine}/binding")
    assert binding["incarnation_id"] is None and binding["execution_epoch"] == source["execution_epoch"]
    assert launch.http(endpoint, launch.EXT + "/operations", request) == saved
    try:
        cancel(launch, endpoint, request, saved["revision"])
    except RuntimeError as error:
        assert "E_ALREADY_COMMITTED" in str(error) or "E_OPERATION_HAS_EFFECTS" in str(error), error
    else:
        raise AssertionError("STOP_COMMIT was reversible")

    for directory in (root / "state/managed" / source["incarnation_id"],
                      root / "local-run/managed" / caps["runtime_instance_id"] / source["incarnation_id"]):
        if directory.is_dir():
            shutil.rmtree(directory)
        assert not directory.exists()
    survivors = holders - {source_pid}
    if with_child:
        survivors.add(child_pid)
    backing_rows, released, retained = measure_release(ram_before, survivors)
    assert released > 0
    durable = root / "catalog/statepoints" / representation
    assert "sha256:" + sha256(durable / "CaptureManifest.json") == saved_point["manifest_digest"]
    durable_root = lookup(database, """
        SELECT s.state,s.durability,p.state FROM resources s JOIN root_refs p USING(resource_id)
        WHERE p.owner_kind='statepoint_hibernate' AND p.owner_id=? AND resource_id=?
    """, (capture, representation))
    assert durable_root == ("available", "durable", "active"), durable_root
    files = [file.stat() for file in durable.rglob("*") if file.is_file()]
    cold_after_exit = child_after = None
    if with_child:
        child_pins = retained_pins(database, branch_id)
        assert retained > 0 and child_pins[0] == "available" and child_pins[2] == 1, child_pins
        assert lookup(database, "SELECT state FROM fd_leases WHERE lease_id=?", (child_lease,)) == ("attached",)
        assert child_backings <= ram_backings([child_pid]).keys()
        cold_after_exit = page_residency(child_pid, cold_addresses)
        assert not any(sample["present"] for sample in cold_after_exit.values()), cold_after_exit
        child_after = ticks(root, child)[-1]
        assert int(child_after[0]) > int(child_before[0]) and child_after[1] == child_before[1]
    else:
        assert retained == 0, backing_rows

    restored = native(launch, endpoint, "vm_restore", "a07-durable-restore", binding,
                      {"capture_id": capture, "mode": "exact", "name": machine})
    resumed = restored["execution"]
    assert resumed["instance_id"] == source["instance_id"] and resumed["execution_epoch"] == source["execution_epoch"] + 1
    assert restored["statepoint"]["manifest_digest"] == saved_point["manifest_digest"]
    volumes = restored["statepoint"]["volume_map"]
    assert set(volumes) == original_volumes and all(old == new for old, new in volumes.items()), volumes
    first = launch.until(lambda: ticks(root, resumed), 25)[0]
    assert first[:2] == [str(int(frozen_ticks[-1][0]) + 1), frozen_ticks[-1][1]], first
    target_pid = execution_inventory(launch, endpoint, resumed)["pid"]
    assert not backing_owners({target_pid}, set(ram_before))

    after_read = None
    if with_child:
        cold, = launch.until(lambda: guest_lines(root, child, "A07_CHILD_COLD_READ "), 65)
        assert cold.split()[1:] == [frozen_ticks[-1][1], str(96 << 20)], cold
        after_read = page_residency(child_pid, cold_addresses)
        assert all(sample["present"] for sample in after_read.values()), after_read

    print(json.dumps({"case": "A07", "bundle": caps["bundle_digest"], "with_live_child": with_child,
        "guest_fixture_sha256": guest_sha, "aborted_export_phase": phase,
        "aborted_export_error": failed, "returned_source": "STANDBY",
        "abort_last_tick": standby_ticks[-1], "wake_first_tick": continued[len(standby_ticks)],
        "captured_phase": exporting, "capture": capture, "durable_manifest": saved_point["manifest_digest"],
        "exit_wait_ms": elapsed, "last_original_tick": frozen_ticks[-1], "first_restored_tick": first,
        "host_disk_fingerprints_at_capture_and_exit": cut_disk, "volatile_export_before": cut_pins,
        "volatile_export_after": retained_pins(database, volatile_id), "durable_root": durable_root,
        "child_capture": branch_id, "child_lease": child_lease,
        "child_tick_before_source_capture": child_before, "child_tick_after_source_exit": child_after,
        "child_pages_before": cold_before, "child_pages_after_source_exit": cold_after_exit,
        "child_pages_after_read": after_read, "logical_ram_bytes": sum(region["length"] for region in point["regions"]),
        "physical_memfd_bytes_before": sum(ram_before.values()), "physical_memfd_bytes_released": released,
        "physical_memfd_bytes_retained": retained,
        "physical_memfd_bytes_shared_with_child": sum(ram_before[identity] for identity in child_backings),
        "memfd_identity_accounting": backing_rows,
        "durable_allocated_file_bytes": sum(file.st_blocks * 512 for file in files),
        "durable_logical_file_bytes": sum(file.st_size for file in files), "restored_volume_ids": list(volumes)},
        sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "smolvm", "guest", "work-dir"):
        parser.add_argument("--" + name, required=True, type=lambda path: Path(path).resolve(strict=True))
    parser.add_argument("--with-child", action="store_true")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM and a systemd manager")
    if os.environ.get("HIBERNATE_CUT_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "HIBERNATE_CUT_NAMESPACE": "1"})
    sys.path.insert(0, str(args.smolvm / "tests"))
    import test_deep_managed_launch as launch

    slice_name = f"smolvmhibcut{os.getpid()}_{os.urandom(4).hex()}.slice"
    subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                    "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                    slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                   check=True, stdout=subprocess.DEVNULL)
    os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
    try:
        with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="hibernate-cut-") as directory:
            root = Path(directory)
            root.chmod(0o755)

            def observer(endpoint, scratch, source, _inventory, _started):
                verify(launch, endpoint, scratch, source, args.with_child, sha256(args.guest))

            try:
                launch.verify(args.bundle, root, args.guest, None, False, False, False, False,
                              observer=observer)
            except BaseException:
                for log in (root / "worker.log", root / "serve.log", *sorted((root / "state/managed").glob("*.log"))):
                    if log.is_file():
                        print(f"{log.name}\n{log.read_text(errors='replace')[-2800:]}", file=sys.stderr)
                raise
    finally:
        subprocess.run(["systemctl", "stop", slice_name], check=True)


if __name__ == "__main__":
    main()
