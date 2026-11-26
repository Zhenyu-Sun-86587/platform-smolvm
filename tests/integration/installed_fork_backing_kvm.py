#!/usr/bin/env python3
"""Trace the common RAM backing and host writes of a real installed OCI Fork batch."""

import argparse
from contextlib import suppress
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import signal
import socket
import sqlite3
import struct
import sys
import tempfile
import time


def peer(path, kind):
    with socket.socket(socket.AF_UNIX, kind) as connection:
        connection.connect(str(path))
        pid, _, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    return pid


def process_io(pid):
    return {name: int(value) for line in Path(f"/proc/{pid}/io").read_text().splitlines()
            for name, value in (line.split(":", 1),)}


def regular_files(root):
    files = {}
    for base in ("catalog", "state", "local-run"):
        for path in (root / base).rglob("*"):
            with suppress(FileNotFoundError):
                if path.is_file() and not path.is_symlink():
                    meta = path.stat()
                    files[str(path.relative_to(root))] = (meta.st_size, meta.st_blocks * 512, meta.st_mtime_ns)
    return files


def proc_maps(pid):
    maps = {}
    for line in Path(f"/proc/{pid}/maps").read_text().splitlines():
        address, permission, offset, group, inode, *_ = line.split(maxsplit=5)
        if permission != "rw-p" or not int(inode):
            continue
        low, high = (int(word, 16) for word in address.split("-"))
        major, minor = (int(word, 16) for word in group.split(":"))
        key = (os.makedev(major, minor), int(inode))
        maps.setdefault(key, []).append((int(offset, 16), high - low, low))
    return maps


def regular_descriptors(pid):
    found = {}
    for path in Path(f"/proc/{pid}/fd").iterdir():
        with suppress(FileNotFoundError):
            meta = path.stat()
            if path.is_file():
                found[(meta.st_dev, meta.st_ino, meta.st_size)] = path
    return found


def backing_descriptors(pid, identities):
    wanted = set(identities.values())
    found = {identity: path for identity, path in regular_descriptors(pid).items() if identity in wanted}
    assert set(found) == wanted, (pid, wanted - set(found))
    return found


def data_ranges(backing, offset, size):
    """Use a new open description so SEEK_DATA cannot change the VMM's file offset."""
    fd = os.open(backing, os.O_RDONLY | os.O_CLOEXEC)
    try:
        seals = fcntl.fcntl(fd, fcntl.F_GET_SEALS)
        required = fcntl.F_SEAL_WRITE | fcntl.F_SEAL_GROW | fcntl.F_SEAL_SHRINK
        assert seals & required == required
        limit = offset + size
        page = os.sysconf("SC_PAGE_SIZE")
        cursor = offset
        while cursor < limit:
            try:
                start = os.lseek(fd, cursor, os.SEEK_DATA)
            except OSError as error:
                if error.errno != errno.ENXIO:
                    raise
                break
            if start >= limit:
                break
            stop = min(os.lseek(fd, start, os.SEEK_HOLE), limit)
            assert cursor <= start < stop and start % page == stop % page == 0
            yield start, stop - start
            cursor = stop
    finally:
        os.close(fd)


def mapped_extent(maps, identity, offset, size):
    """Account for a backing range even when Linux splits it into several VMAs."""
    start = offset
    end = offset + size
    output = []
    for position, length, address in sorted(maps.get(identity[:2], [])):
        stop = position + length
        if position <= start < stop:
            take = min(end, stop) - start
            output.append({"offset": start, "length": take, "vaddr": address + start - position})
            start += take
            if start == end:
                return output
    raise AssertionError(("backing range has no complete private target mapping", identity, offset, size, output))


def capture_catalog(database, capture_id, child_receipts):
    with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as conn:
        conn.execute("BEGIN")
        rows = conn.execute("""
            SELECT t.descriptor_json, r.durability, r.immutable, t.transfer_id
            FROM fd_transfers t JOIN resources r USING(resource_id)
            WHERE r.resource_id=? AND r.state='available' AND t.state='committed'
        """, (capture_id,)).fetchall()
        row, = rows
        descriptor, durability, immutable, transfer = row
        assert durability == "volatile" and immutable == 1, row
        identities = {}
        for (values,) in conn.execute(
            "SELECT identities_json FROM fd_transfer_batches WHERE transfer_id=?", (transfer,)
        ):
            for backing, device, inode, length in json.loads(values):
                assert backing not in identities
                identities[backing] = (device, inode, length)
        captures = {}
        for child in child_receipts:
            lease, = conn.execute("""
                SELECT l.lease_id, l.delivered
                FROM fd_leases l JOIN root_refs r ON r.owner_id=l.lease_id AND r.resource_id=l.resource_id
                WHERE l.operation_id=? AND l.incarnation_id=? AND l.resource_id=? AND l.state='attached'
                AND r.owner_kind='fd_lease' AND r.state='active'
            """, (child["operation_id"], child["incarnation_id"], capture_id)).fetchall()
            assert lease[1] == len(identities), (child, lease)
            captures[child["incarnation_id"]] = lease[0]
        durable_points, = conn.execute(
            "SELECT count(*) FROM statepoints WHERE capture_id=?", (capture_id,)
        ).fetchone()
        assert durable_points == 0
    assert identities and set(identities) == set(json.loads(descriptor)["backing_sizes"])
    return json.loads(descriptor), identities, captures


def invoke_fork(launch, endpoint, source, key, names):
    batch = {"operation_id": key, "request_digest": launch.digest(key.encode()),
             "kind": "fork_batch", "expected": source, "parameters": {"children": names}}
    submitted = launch.http(endpoint, launch.EXT + "/operations", batch)
    completed = launch.settled(endpoint, key)
    assert submitted["phase"] == "NEW" and completed["phase"] == "COMPLETED", completed
    receipt = completed["fork"]
    assert submitted["fork"]["capture_id"] == receipt["capture_id"] and receipt["source_released"]
    children = receipt["children"]
    assert len(children) == len(names) and all(child["phase"] == "COMPLETED" for child in children)
    return receipt


def execution_inventory(launch, endpoint, execution):
    observed = launch.http(endpoint, f"{launch.EXT}/instances/{execution['instance_id']}/inventory")
    assert observed["execution"]["incarnation_id"] == execution["incarnation_id"]
    return observed["execution"]


def verify_batch(launch, endpoint, root, source, inventory, guest_digest):
    bundle = launch.http(endpoint, launch.EXT + "/capabilities")["bundle_digest"]
    existing = {"source": inventory["execution"]["pid"], "serve": peer(endpoint, socket.SOCK_STREAM),
                "worker": peer(root / "ipc/worker.sock", socket.SOCK_SEQPACKET)}
    original_backings = regular_descriptors(existing["source"])
    initial_io = {role: process_io(pid) for role, pid in existing.items()}
    before = regular_files(root)
    key = "observe-installed-fork"
    start = time.monotonic_ns()
    receipt = invoke_fork(launch, endpoint, source, key, ["backing-first", "backing-second"])
    elapsed = time.monotonic_ns() - start
    capture_id = receipt["capture_id"]
    children = receipt["children"]
    proc = dict(existing)
    for child in children:
        child["pid"] = proc[child["name"]] = execution_inventory(launch, endpoint, child)["pid"]
    final_io = {role: process_io(pid) for role, pid in proc.items()}
    changes = {role: {key: values[key] - initial_io.get(role, {}).get(key, 0)
                       for key in ("write_bytes", "wchar", "syscw", "cancelled_write_bytes")}
               for role, values in final_io.items()}
    after = regular_files(root)
    written = sum(values["write_bytes"] for values in changes.values())
    descriptor, identities, leases = capture_catalog(root / "catalog/catalog.sqlite", capture_id, children)
    assert set(identities.values()) <= original_backings.keys(), (identities, original_backings)
    backings = backing_descriptors(proc["worker"], identities)
    extents = []
    for region in descriptor["regions"]:
        for extent in region["extents"]:
            backing = identities[extent["backing_id"]]
            end = extent["backing_offset"] + extent["length"]
            assert end <= backing[2]
            extents.append(({**extent, "gpa": region["gpa"] + extent["offset"],
                             "device": backing[0], "inode": backing[1], "backing_length": backing[2]},
                            backing, list(data_ranges(backings[backing], extent["backing_offset"], extent["length"]))))

    recorded = []
    ram = sum(region["length"] for region in descriptor["regions"])
    assert ram >= 512 * 1024 * 1024 and 0 <= written < ram, (ram, written, changes)
    assert sum(values["wchar"] for values in changes.values()) < ram, changes
    for child in children:
        maps = proc_maps(child["pid"])
        backing_descriptors(child["pid"], identities)
        observed = [{**extent, "file_mappings": [mapping for offset, length in data
                                                 for mapping in mapped_extent(maps, backing, offset, length)],
                     "sparse_zero_bytes": extent["length"] - sum(length for _, length in data)}
                    for extent, backing, data in extents]
        assert sum(entry["length"] for entry in observed) == ram
        recorded.append({**child, "capture_id": capture_id, "active_reader_lease": leases[child["incarnation_id"]],
                         "mapped_extents": observed})
    assert len({child["active_reader_lease"] for child in recorded}) == 2
    assert not any(path.startswith("catalog/statepoints/") for path in after)
    assert all(values[1] < ram for path, values in after.items() if before.get(path) != values)
    print(json.dumps({
        "case": "A01", "bundle": bundle, "capture_id": capture_id, "guest_fixture_sha256": guest_digest,
        "batch_operation": key, "elapsed_ms": elapsed / 1e6,
        "ram_bytes_per_child": ram, "backing_count": len(identities), "children": recorded,
        "source_backings_before_capture": {name: {"device": device, "inode": inode, "size": size}
                                          for name, (device, inode, size) in identities.items()},
        "data_mapping_bytes_per_child": sum(length for _, _, data in extents for _, length in data),
        "io_delta_capture_to_publication": changes, "observed_write_bytes_upper_bound": written,
        "changed_files": {path: {"logical_bytes": values[0], "allocated_bytes": values[1]}
                          for path, values in after.items() if before.get(path) != values},
    }, sort_keys=True), flush=True)
    return bundle, receipt


def guest_lines(root, reference, prefix):
    log = root / "state/managed" / (reference["incarnation_id"] + ".log")
    if not log.exists():
        return []
    return [line for line in log.read_text(errors="replace").splitlines() if line.startswith(prefix)]


def fence_and_remove(launch, endpoint, root, serve, name, reference):
    identity = execution_inventory(launch, endpoint, reference)
    incarnation = reference["incarnation_id"]
    runtime_dir = root / "local-run/managed" / serve / incarnation
    state_dir = root / "state/managed" / incarnation
    assert runtime_dir.is_dir() and state_dir.is_dir() and (runtime_dir / "c.sock").is_socket()
    fd = os.pidfd_open(identity["pid"])
    try:
        start = int(Path(f"/proc/{identity['pid']}/stat").read_text().rsplit(")", 1)[1].split()[19])
        assert start == identity["pid_start_time"], identity
        poll = select.poll()
        poll.register(fd, select.POLLIN)
        signal.pidfd_send_signal(fd, signal.SIGKILL)
        assert poll.poll(10_000), identity
    finally:
        os.close(fd)
    launch.until(lambda: launch.http(endpoint, f"{launch.EXT}/machines/{name}/binding")["incarnation_id"] is None, 20)
    for directory in (runtime_dir, state_dir):
        if directory.is_dir():
            shutil.rmtree(directory)
        assert not directory.exists()
    assert not (runtime_dir / "c.sock").exists()
    return {"name": name, "incarnation_id": incarnation, "pid": identity["pid"],
            "removed_runtime_dir": str(runtime_dir.relative_to(root)),
            "removed_state_dir": str(state_dir.relative_to(root)), "removed_native_socket": True}


def host_page_address(descriptor, identities, maps, gpa):
    page_size = os.sysconf("SC_PAGE_SIZE")
    for region in descriptor["regions"]:
        for extent in region["extents"]:
            start = region["gpa"] + extent["offset"]
            if start <= gpa and gpa + page_size <= start + extent["length"]:
                backing = identities[extent["backing_id"]]
                offset = extent["backing_offset"] + gpa - start
                mapping, = mapped_extent(maps, backing, offset, page_size)
                return mapping["vaddr"], backing[1], offset
    raise AssertionError(("Guest physical page is outside the capture", gpa))


def page_residency(pid, addresses):
    page_size = os.sysconf("SC_PAGE_SIZE")
    descriptor = os.open(f"/proc/{pid}/pagemap", os.O_RDONLY | os.O_CLOEXEC)
    try:
        samples = {}
        for gpa, (address, inode, offset) in addresses.items():
            raw = os.pread(descriptor, 8, address // page_size * 8)
            assert len(raw) == 8
            samples[gpa] = {"present": bool(int.from_bytes(raw, sys.byteorder) >> 63),
                            "inode": inode, "backing_offset": offset}
        return samples
    finally:
        os.close(descriptor)


def verify_lineage(launch, endpoint, root, source, bundle, first, guest_digest):
    seed, = launch.until(lambda: guest_lines(root, source, "A02_COLD_GPAS "), 10)
    gpas = [int(value) for value in seed.split()[1:]]
    assert len(set(gpas)) == 6 and min(gpas) > 0
    branch, sibling = first["children"]
    changed, = launch.until(lambda: guest_lines(root, branch, "A02_LAYER_ONE "), 10)
    source_guest_pid, unchanged_ram, changed_ram = map(int, changed.split()[1:])
    assert (unchanged_ram, changed_ram) == (56 << 20, 8 << 20)
    serve = launch.http(endpoint, launch.EXT + "/capabilities")["runtime_instance_id"]

    ancestors = [fence_and_remove(launch, endpoint, root, serve, "test-managed", source)]
    branch_ref = {field: branch[field] for field in ("instance_id", "incarnation_id", "execution_epoch")}
    second = invoke_fork(launch, endpoint, branch_ref, "fork-after-source-removal", ["cold-leaf"])
    leaf, = second["children"]
    leaf_pid = execution_inventory(launch, endpoint, leaf)["pid"]
    armed, = launch.until(lambda: guest_lines(root, leaf, "A02_COLD_LEAF_ARMED "), 15)
    assert int(armed.split()[1]) == source_guest_pid
    descriptor, identities, _ = capture_catalog(root / "catalog/catalog.sqlite", second["capture_id"], [leaf])
    maps = proc_maps(leaf_pid)
    addresses = {gpa: host_page_address(descriptor, identities, maps, gpa) for gpa in gpas}

    ancestors.append(fence_and_remove(launch, endpoint, root, serve, "backing-first", branch))
    removed_sibling = fence_and_remove(launch, endpoint, root, serve, "backing-second", sibling)
    assert not guest_lines(root, leaf, "A02_COLD_READ ")
    before = page_residency(leaf_pid, addresses)
    cold = {gpa for gpa, sample in before.items() if not sample["present"]}
    assert cold, before
    checked, = launch.until(lambda: guest_lines(root, leaf, "A02_COLD_READ "), 40)
    assert [int(value) for value in checked.split()[1:]] == [source_guest_pid, 64 << 20]
    disk = [list(map(int, row.split()[1:])) for row in guest_lines(root, leaf, "A02_DISK ")]
    assert disk == [[0, 8229, 8246, 160, 192], [1, 8230, 8247, 161, 193]], disk
    after = page_residency(leaf_pid, addresses)
    assert all(after[gpa]["present"] for gpa in cold), (before, after)

    leaf_ref = {field: leaf[field] for field in ("instance_id", "incarnation_id", "execution_epoch")}
    third = invoke_fork(launch, endpoint, leaf_ref, "fork-after-cold-read", ["cold-descendant"])
    child, = third["children"]
    descendant, = launch.until(lambda: guest_lines(root, child, "A02_DESCENDANT "), 20)
    assert int(descendant.split()[1]) == source_guest_pid
    print(json.dumps({
        "case": "A02", "bundle": bundle, "guest_fixture_sha256": guest_digest,
        "capture_chain": [first["capture_id"], second["capture_id"], third["capture_id"]],
        "removed_ancestors": ancestors, "removed_sibling": removed_sibling,
        "leaf_incarnation_id": leaf["incarnation_id"], "leaf_host_pid": leaf_pid,
        "guest_pid": source_guest_pid, "verified_ram_bytes": 64 << 20,
        "original_ram_bytes": unchanged_ram, "changed_ram_bytes": changed_ram,
        "sampled_host_pages_before_first_read": before, "sampled_host_pages_after_first_read": after,
        "disks_and_offsets": [{"disk": index, "before": first_pos, "after": final_pos,
                               "original_byte": original, "child_byte": new}
                              for index, first_pos, final_pos, original, new in disk],
        "descendant_incarnation_id": child["incarnation_id"], "descendant_guest_proof": descendant,
    }, sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "smolvm", "guest"):
        parser.add_argument("--" + name, required=True, type=lambda p: Path(p).resolve(strict=True))
    parser.add_argument("--lineage", action="store_true", help="also verify cold lineage using cold-fork-guest.c")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, a Linux systemd manager and KVM")
    if os.environ.get("FORK_BACKING_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "FORK_BACKING_KVM_NAMESPACE": "1"})
    sys.path.insert(0, str(args.smolvm / "tests"))
    import test_deep_managed_launch as launch

    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="fork-backing-") as directory:
        root = Path(directory)
        root.chmod(0o755)
        with args.guest.open("rb") as guest:
            guest_digest = hashlib.file_digest(guest, "sha256").hexdigest()

        def observer(endpoint, scratch, source, inventory, _):
            bundle, batch = verify_batch(launch, endpoint, scratch, source, inventory, guest_digest)
            if args.lineage:
                verify_lineage(launch, endpoint, scratch, source, bundle, batch, guest_digest)

        try:
            launch.verify(args.bundle, root, args.guest, None, False, False, False, False,
                          observer=observer)
        except BaseException:
            for log in (root / "worker.log", root / "serve.log", *sorted((root / "state/managed").glob("*.log"))):
                if log.is_file():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-5000:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
