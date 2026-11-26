#!/usr/bin/env python3
"""Exercise independent writes, exact resume and last-reference collection on installed KVM."""

import argparse
from contextlib import suppress
import hashlib
import json
import os
from pathlib import Path
import select
import socket
import sqlite3
import subprocess
import sys
import tempfile

from installed_fork_backing_kvm import (
    capture_catalog,
    execution_inventory,
    fence_and_remove,
    guest_lines,
    invoke_fork,
    peer,
    regular_descriptors,
)


def execution(reference):
    return {key: reference[key] for key in ("instance_id", "incarnation_id", "execution_epoch")}


def clock_sample(root, reference):
    assert not guest_lines(root, reference, "A03_MISMATCH ")
    rows = guest_lines(root, reference, "PARKED_BLOCK_TICK ")
    if rows:
        _, tick, pid, name, memory, first, second = rows[-1].split()
        assert int(first, 16) == int(memory, 16)
        assert int(second, 16) == int(memory, 16) ^ 0x0123456789ABCDEF
        return {"tick": int(tick), "pid": int(pid), "name": name,
                "memory": memory, "disk0": first, "disk1": second}


def next_ticks(launch, root, reference, before, count=3):
    def advanced():
        measured = clock_sample(root, reference)
        if measured and measured["tick"] >= before["tick"] + count:
            assert measured["pid"] == before["pid"] and measured["name"] == before["name"]
            return measured

    return launch.until(advanced, 20)


def backing_owners(pids, identities):
    """A mapping can keep an unlinked memfd alive after its descriptor closes."""
    mapping_keys = {identity[:2] for identity in identities}
    owners = []
    for pid in sorted(pids):
        process = Path("/proc") / str(pid)
        with suppress(FileNotFoundError, ProcessLookupError):
            maps = set()
            for line in (process / "maps").read_text().splitlines():
                _, _, _, group, inode, *_ = line.split(maxsplit=5)
                if int(inode):
                    major, minor = (int(word, 16) for word in group.split(":"))
                    maps.add((os.makedev(major, minor), int(inode)))
            used_descriptors = identities & regular_descriptors(pid).keys()
            used_mappings = mapping_keys & maps
            if used_descriptors or used_mappings:
                owners.append({"pid": pid,
                               "descriptor_inodes": sorted(inode for _, inode, _ in used_descriptors),
                               "mapped_inodes": sorted(inode for _, inode in used_mappings)})
    return owners


def native(launch, endpoint, kind, operation_id, expected, parameters):
    request = {"kind": kind, "operation_id": operation_id, "request_digest": launch.digest(operation_id.encode()),
               "expected": execution(expected), "parameters": parameters}
    accepted = launch.http(endpoint, launch.EXT + "/operations", request)
    assert accepted["phase"] == "NEW", accepted
    result = launch.settled(endpoint, operation_id, 160)
    assert result["phase"] == "COMPLETED", result
    return result


def attached_volumes(launch, endpoint, reference):
    info = launch.http(endpoint, f"{launch.EXT}/instances/{reference['instance_id']}/inventory")
    return {entry["block"]["managed_attachment"]["device_id"]: entry["block"]["managed_attachment"]["volume_id"]
            for entry in info["inventory"]["devices"] if entry.get("block")}


def graph(database, captures):
    rows = []
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as reader:
        for resource in captures:
            nodes = reader.execute("""
                SELECT b.operation_id,o.phase,b.source_incarnation,c.ordinal,c.name,c.instance_id,c.operation_id,
                       m.incarnation_id,o_child.phase
                FROM managed_fork_batches b JOIN operations o ON o.operation_id=b.operation_id
                JOIN managed_fork_children c ON c.batch_id=b.operation_id
                JOIN managed_launches m ON m.operation_id=c.operation_id
                JOIN operations o_child ON o_child.operation_id=c.operation_id
                WHERE b.capture_id=? ORDER BY c.ordinal
            """, (resource,)).fetchall()
            assert nodes and all(node[1] == node[-1] == "COMPLETED" for node in nodes), nodes
            rows.append({"capture_id": resource, "source_incarnation": nodes[0][2],
                         "operation_id": nodes[0][0],
                         "children": [{"name": node[4], "instance_id": node[5],
                                       "operation_id": node[6], "incarnation_id": node[7]} for node in nodes]})
    return rows


def reclamation_state(database, captures):
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as reader:
        return {resource: reader.execute("""
                SELECT r.state,t.state,
                       (SELECT count(*) FROM root_refs x WHERE x.resource_id=r.resource_id AND x.state='active'),
                       (SELECT count(*) FROM fd_leases l WHERE l.resource_id=r.resource_id AND l.state!='released')
                FROM resources r JOIN fd_transfers t USING(resource_id) WHERE r.resource_id=?
                """, (resource,)).fetchone() for resource in captures}


def observe(launch, endpoint, root, source, guest_checksum):
    caps = launch.http(endpoint, launch.EXT + "/capabilities")
    serve = caps["runtime_instance_id"]
    database = root / "catalog/catalog.sqlite"
    worker = root / "ipc/worker.sock"
    broker_pid = peer(worker, socket.SOCK_SEQPACKET)
    serve_pid = peer(endpoint, socket.SOCK_STREAM)

    first = invoke_fork(launch, endpoint, source, "a03-root-fork", ["a03-left", "a03-right"])
    left, right = first["children"]
    _, base_identities, _ = capture_catalog(database, first["capture_id"], [left, right])
    original_actors = (("source", source), ("left", left), ("right", right))
    overlapping = {name: launch.until(lambda ref=ref: clock_sample(root, ref), 15)
                   for name, ref in original_actors}
    later = {name: next_ticks(launch, root, ref, overlapping[name], 4) for name, ref in original_actors}
    assert len({result["memory"][:4] for result in later.values()}) == 3, later

    removed = [fence_and_remove(launch, endpoint, root, serve, "test-managed", source)]
    second = invoke_fork(launch, endpoint, execution(left), "a03-left-fork", ["a03-middle"])
    middle, = second["children"]
    _, middle_identities, _ = capture_catalog(database, second["capture_id"], [middle])
    middle_live = launch.until(lambda: clock_sample(root, middle), 15)
    assert middle_live["pid"] == later["source"]["pid"]
    middle_later = next_ticks(launch, root, middle, middle_live)
    removed.append(fence_and_remove(launch, endpoint, root, serve, "a03-left", left))

    third = invoke_fork(launch, endpoint, execution(middle), "a03-middle-fork", ["a03-leaf"])
    leaf, = third["children"]
    _, leaf_identities, _ = capture_catalog(database, third["capture_id"], [leaf])
    leaf_live = launch.until(lambda: clock_sample(root, leaf), 15)
    assert leaf_live["pid"] == later["source"]["pid"]
    leaf_later = next_ticks(launch, root, leaf, leaf_live)
    removed.append(fence_and_remove(launch, endpoint, root, serve, "a03-middle", middle))
    right_survived = next_ticks(launch, root, right, later["right"])
    captures = [point["capture_id"] for point in (first, second, third)]
    genealogy = graph(database, captures)
    assert [node["source_incarnation"] for node in genealogy] == [
        source["incarnation_id"], left["incarnation_id"], middle["incarnation_id"]]

    pid = execution_inventory(launch, endpoint, leaf)["pid"]
    original_volumes = attached_volumes(launch, endpoint, leaf)
    fd = os.pidfd_open(pid)
    point_id = "a03-independent-hibernate"
    try:
        saved = native(launch, endpoint, "hibernate", "a03-leaf-sleep", leaf,
                       {"capture_id": point_id})["statepoint"]
        poll = select.poll()
        poll.register(fd, select.POLLIN)
        assert poll.poll(10_000), saved
    finally:
        os.close(fd)
    assert saved["phase"] == "hibernated" and saved["source_released"] and saved["capture_id"] == point_id
    prior_leaf = clock_sample(root, leaf)
    binding = launch.http(endpoint, f"{launch.EXT}/machines/a03-leaf/binding")
    assert binding["incarnation_id"] is None and binding["execution_epoch"] == leaf["execution_epoch"]
    right_asleep = next_ticks(launch, root, right, right_survived)

    restored = native(launch, endpoint, "vm_restore", "a03-leaf-exact-restore", binding,
                      {"name": "a03-leaf", "capture_id": point_id, "mode": "exact"})
    result = restored["statepoint"]
    assert result["stage"] == "released" and result["manifest_digest"] == saved["manifest_digest"], result
    assert len(original_volumes) >= 2 and set(result["volume_map"]) == set(original_volumes.values()), result
    assert all(a == b for a, b in result["volume_map"].items()), result
    now = restored["execution"]
    assert now["instance_id"] == leaf["instance_id"] and now["incarnation_id"] != leaf["incarnation_id"]
    assert now["execution_epoch"] == leaf["execution_epoch"] + 1
    assert attached_volumes(launch, endpoint, now) == original_volumes
    first_after = launch.until(lambda: guest_lines(root, now, "PARKED_BLOCK_TICK "), 25)[0]
    assert first_after.split()[1:4] == [str(prior_leaf["tick"] + 1), str(prior_leaf["pid"]), "a03-leaf"]
    leaf_awake = launch.until(lambda: clock_sample(root, now), 20)
    next_ticks(launch, root, right, right_asleep)

    all_backings = set(base_identities.values()) | set(middle_identities.values()) | set(leaf_identities.values())
    right_pid = execution_inventory(launch, endpoint, right)["pid"]
    fresh_pid = execution_inventory(launch, endpoint, now)["pid"]
    assert not (all_backings & regular_descriptors(fresh_pid).keys())
    retained = set(base_identities.values()) & regular_descriptors(broker_pid).keys() & regular_descriptors(right_pid).keys()
    assert retained == set(base_identities.values()), (retained, base_identities)
    before_gc = reclamation_state(database, captures)
    assert before_gc[captures[0]][:2] == ("available", "committed"), before_gc
    retained_files = regular_descriptors(broker_pid)
    blocks_before = 0
    for identity in retained:
        information = retained_files[identity].stat()
        assert information.st_nlink == 0
        blocks_before += information.st_blocks * 512
    assert blocks_before > 0
    live_processes = {broker_pid, serve_pid, right_pid, fresh_pid}
    owners_before = backing_owners(live_processes, all_backings)
    assert {broker_pid, right_pid} <= {owner["pid"] for owner in owners_before}

    last = fence_and_remove(launch, endpoint, root, serve, "a03-right", right)

    def collected():
        current = reclamation_state(database, captures)
        if all(row == ("deleted", "collected", 0, 0) for row in current.values()):
            return current

    after_gc = launch.until(collected, 20)
    launch.until(lambda: not backing_owners(live_processes - {right_pid}, all_backings), 20)
    owners_after = backing_owners(live_processes - {right_pid}, all_backings)
    assert not owners_after, owners_after
    assert graph(database, captures) == genealogy
    durable = launch.rpc(worker, "QueryResource", {"resource_id": saved["representation_id"]})
    assert durable["state"] == "available" and durable["durability"] == "durable", durable
    leaf_finished = next_ticks(launch, root, now, leaf_awake)
    print(json.dumps({
        "case": "A03", "bundle": caps["bundle_digest"], "guest_fixture_sha256": guest_checksum,
        "simultaneous_source_and_sibling_writes_before": overlapping, "simultaneous_writes_after": later,
        "middle_branch_writes": [middle_live, middle_later], "leaf_branch_writes": [leaf_live, leaf_later],
        "removed_ancestors": removed, "lineage_before_and_after_collection": genealogy,
        "right_sibling_progress_while_leaf_hibernated": right_asleep, "saved_checkpoint": point_id,
        "durable_manifest_digest": saved["manifest_digest"], "durable_representation_id": saved["representation_id"],
        "original_leaf": execution(leaf), "restored_leaf": now, "guest_before_restore": prior_leaf,
        "guest_first_after_restore": first_after, "exact_writable_volumes": result["volume_map"],
        "last_volatile_reader": last, "worker_and_sibling_retained_backing_inodes": sorted(inode for _, inode, _ in retained),
        "volatile_before": before_gc, "volatile_after": after_gc, "restored_leaf_after_collection": leaf_finished,
        "volatile_backing_owners_before": owners_before, "volatile_backing_owners_after": owners_after,
        "allocated_bytes_in_final_unlinked_backings": blocks_before,
    }, sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "smolvm", "guest"):
        parser.add_argument("--" + name, required=True, type=lambda p: Path(p).resolve(strict=True))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, Linux KVM and a systemd manager")
    if os.environ.get("FORK_LINEAGE_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "FORK_LINEAGE_KVM_NAMESPACE": "1"})
    sys.path.insert(0, str(args.smolvm / "tests"))
    import test_deep_managed_launch as launch

    slice_name = f"smolvmlineage{os.getpid()}_{os.urandom(4).hex()}.slice"
    subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                    "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                    slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                   check=True, stdout=subprocess.DEVNULL)
    os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
    try:
        with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="fork-lineage-") as directory:
            root = Path(directory)
            root.chmod(0o755)
            with args.guest.open("rb") as guest:
                checksum = hashlib.file_digest(guest, "sha256").hexdigest()

            def observer(endpoint, scratch, source, _, __):
                observe(launch, endpoint, scratch, source, checksum)

            try:
                launch.verify(args.bundle, root, args.guest, None, False, False, False, False,
                              observer=observer)
            except BaseException:
                for log in (root / "worker.log", root / "serve.log", *sorted((root / "state/managed").glob("*.log"))):
                    if log.is_file():
                        print(f"{log.name}\n{log.read_text(errors='replace')[-3200:]}", file=sys.stderr)
                raise
    finally:
        subprocess.run(["systemctl", "stop", slice_name], check=True)


if __name__ == "__main__":
    main()
