#!/usr/bin/env python3
"""Compare ordinary UDP delivery after two successive Forks of one owned System Guest."""

import argparse
from contextlib import ExitStack, closing
from dataclasses import dataclass
import fcntl
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import time

import owned_gate_cut_kvm as gate
from managed_system_vsock import Platform, digest


GSTATS = r"""
printf 'last_send '; cat /run/owned-gate-last-send
ip -s link show dev eth0
tc -s qdisc show dev eth0
for field in inflight limit; do
    path=/sys/class/net/eth0/queues/tx-0/byte_queue_limits/$field
    if test -r "$path"; then printf 'bql_%s ' "$field"; cat "$path"; fi
done
cat /proc/interrupts
"""


def log(event, **data):
    print(event, json.dumps(data, sort_keys=True), flush=True)


def checkpoint(platform, operation):
    with gate.catalog_reader(platform) as db:
        row, = db.execute("""
            SELECT b.capture_id,b.source_incarnation,b.checkpoint,t.descriptor_json,t.transfer_id
            FROM managed_fork_batches b JOIN fd_transfers t ON t.resource_id=b.capture_id
            WHERE b.operation_id=? AND b.source_released=1 AND t.state='committed'
        """, (operation,)).fetchall()
        backings = {name: (device, inode, size)
                    for batch, in db.execute("SELECT identities_json FROM fd_transfer_batches WHERE transfer_id=?",
                                             (row["transfer_id"],))
                    for name, device, inode, size in json.loads(batch)}
    raw = row["checkpoint"]
    cursor = 16 if raw.startswith(b"SMVCSBY1") else 0

    def count():
        nonlocal cursor
        size, = struct.unpack_from("<I", raw, cursor)
        cursor += 4
        return size

    def section():
        nonlocal cursor
        size = count()
        start = cursor
        cursor += size
        assert cursor <= len(raw), cursor
        return raw[start:cursor]

    section()
    cpus = count()
    assert cpus == 1, cpus
    section()
    state = json.loads(section())
    (slot, network), = [(i, device["TapNet"]) for i, device in enumerate(state["devices"]) if "TapNet" in device]
    queues = {direction: network["queue_" + direction] for direction in ("rx", "tx")}
    fields = {}
    for direction, queue in queues.items():
        size = queue["size"]
        assert queue["ready"] and 2 <= size <= 4096 and (size & (size - 1)) == 0, queue
        fields[direction] = {
            "avail": queue["avail_ring"] + 2, "used": queue["used_ring"] + 2,
            "used_event": queue["avail_ring"] + 4 + 2 * size,
            "avail_event": queue["used_ring"] + 4 + 8 * size,
        }
    addresses = tuple(address for queue in fields.values() for address in queue.values())
    descriptor = json.loads(row["descriptor_json"])
    assert {name: identity[2] for name, identity in backings.items()} == descriptor["backing_sizes"]
    region, = [r for r in descriptor["regions"] if
               all(r["gpa"] <= address and address + 2 <= r["gpa"] + r["length"] for address in addresses)]
    log("Checkpoint", operation=operation, capture_id=row["capture_id"], source_incarnation=row["source_incarnation"],
        sha256=hashlib.sha256(raw).hexdigest(), queues=queues, irq_status=state["interrupt_status"][slot],
        ram_extent_counts=[len(r["extents"]) for r in descriptor["regions"]],
        ring_region={"gpa": region["gpa"], "length": region["length"]})
    return fields, region, backings, row["capture_id"]


@dataclass(frozen=True)
class Mapping:
    start: int
    end: int
    offset: int
    device: str
    inode: int

    def covers(self, address):
        return self.start <= address and address + 2 <= self.end


def maps(owner):
    verify_execution(owner)
    result = []
    for line in (Path("/proc") / str(owner["pid"]) / "maps").read_text().splitlines():
        pieces = line.split(None, 5)
        if len(pieces) != 6 or pieces[5] != "/memfd:smolvm-guest-ram (deleted)":
            continue
        span, _, offset, device, inode, _ = pieces
        start, end = (int(bound, 16) for bound in span.split("-"))
        result.append(Mapping(start, end, int(offset, 16), device, int(inode)))
    return result


def verify_execution(owner):
    proc = Path("/proc") / str(owner["pid"])
    start = int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19])
    assert start == owner["pid_start_time"], owner
    assert any(line.startswith("0::/" + os.environ["SMOLVM_WORKER_SLICE_NAME"] + "/")
               for line in (proc / "cgroup").read_text().splitlines()), owner


def sender(platform, machine, ref):
    execution = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]
    assert gate.identity(execution) == gate.identity(ref) and execution["phase"] == "registered", execution
    assert execution["bundle_digest"] == platform.bundle_id, execution
    assert execution["host_boot_id"] == Path("/proc/sys/kernel/random/boot_id").read_text().strip(), execution
    owner = dict(machine=machine, ref=gate.identity(ref), ip=gate.endpoint(platform, machine)["private_ipv4"],
                 guest_identity=gate.guest_identity(platform, ref), pid=execution["pid"], pid_start_time=execution["pid_start_time"])
    verify_execution(owner)
    return owner


def live_rings(owner, fields, displacement, owned_maps=None):
    owned_maps = maps(owner) if owned_maps is None else owned_maps
    path = Path("/proc") / str(owner["pid"]) / "mem"
    with closing(os.fdopen(os.open(path, os.O_RDONLY | os.O_CLOEXEC), "rb", buffering=0)) as memory:
        data = {}
        for direction, queue in fields.items():
            data[direction] = {}
            for field, gpa in queue.items():
                address = gpa + displacement
                assert any(vma.covers(address) for vma in owned_maps), (owner["machine"], direction, field)
                data[direction][field], = struct.unpack("<H", os.pread(memory.fileno(), 2, address))
    verify_execution(owner)
    return data


def backing_position(address, region, backings):
    relative = address - region["gpa"]
    extent, = [part for part in region["extents"]
               if part["offset"] <= relative and relative + 2 <= part["offset"] + part["length"]]
    offset = extent["backing_offset"] + relative - extent["offset"]
    return backings[extent["backing_id"]], offset


def ring_mapping(owner, fields, region, backings):
    owned_maps = maps(owner)
    addresses = {address for ring in fields.values() for address in ring.values()}
    matched = []
    for address in addresses:
        (device, inode, _), offset = backing_position(address, region, backings)
        device_id = f"{os.major(device):02x}:{os.minor(device):02x}"
        candidates = {vma.start + offset - vma.offset - address
                      for vma in owned_maps if (vma.device, vma.inode) == (device_id, inode)
                      and vma.offset <= offset and vma.covers(vma.start + offset - vma.offset)}
        if candidates:
            matched.append(candidates)
    assert len(matched) == len(addresses), (owner["machine"], len(matched), len(addresses))
    displacement, = set.intersection(*matched)
    log("RingMapping", machine=owner["machine"], matched_positions=len(matched), ram_mappings=len(owned_maps))
    return displacement


def sealed_rings(platform, fields, region, backings):
    proc = Path("/proc") / str(platform.broker.pid)
    assert os.fsencode(platform.root / "catalog") in (proc / "cmdline").read_bytes().split(b"\0")
    handles = {}
    for path in (proc / "fd").iterdir():
        try:
            stat = path.stat()
            identity = stat.st_dev, stat.st_ino, stat.st_size
            if identity in backings.values():
                handles[identity] = path
        except FileNotFoundError:
            continue
    data = {}
    for direction, queue in fields.items():
        data[direction] = {}
        for field, address in queue.items():
            identity, offset = backing_position(address, region, backings)
            if path := handles.get(identity):
                with path.open("rb", buffering=0) as memory:
                    stat = os.fstat(memory.fileno())
                    assert (stat.st_dev, stat.st_ino, stat.st_size) == identity
                    assert fcntl.fcntl(memory, fcntl.F_GET_SEALS) & fcntl.F_SEAL_WRITE
                    data[direction][field], = struct.unpack("<H", os.pread(memory.fileno(), 2, offset))
    return data


def collector_rows(platform, receiver, clients):
    data = {name: [] for name in clients}
    by_ip = {sender["ip"]: name for name, sender in clients.items()}
    for line in platform.guest(receiver, ["cat", gate.RECEIPTS]).decode().splitlines():
        ip, token, seq, hostname, machine_id = line.split("\t")
        if name := by_ip.get(ip):
            assert (hostname, machine_id) == clients[name]["guest_identity"], line
            data[name].append((int(seq), token))
    return data


def window(platform, receiver, clients, fields, displacements, label):
    started = time.monotonic()
    counts = []
    for second in (0, 6, 12, 18):
        time.sleep(max(0, started + second - time.monotonic()))
        elapsed = time.monotonic() - started
        rings = {}
        for name, owner in clients.items():
            if name in displacements:
                try:
                    rings[name] = live_rings(owner, fields, displacements[name])
                except (AssertionError, OSError) as error:
                    rings[name] = {"unavailable": str(error)}
        guest = {name: platform.guest(owner["ref"], ["/bin/sh", "-ec", GSTATS]).decode() for name, owner in clients.items()}
        rows = collector_rows(platform, receiver, clients)
        summary = {name: {"count": len(records), "recent": records[-4:]} for name, records in rows.items()}
        counts.append({name: value["count"] for name, value in summary.items()})
        log("Window", after=label, elapsed=round(elapsed, 3), receipts=summary, rings=rings, guest=guest)
    totals = {name: [sample[name] for sample in counts] for name in clients}
    assert totals["source"][-1] > totals["source"][0], totals
    return totals


def run(platform):
    receiver, _, receiver_ref = gate.machine(platform, "trace-collector", "native")
    sink = gate.endpoint(platform, receiver)["private_ipv4"]
    gate.run_program(platform, receiver_ref, "owned-gate-collector", "collect")
    machine, _, ref = gate.machine(platform, "trace-source", "fixed_internal")
    source = sender(platform, machine, ref)
    gate.run_program(platform, ref, "owned-gate-workload", "serve", sink)
    clients = {"source": source}
    gate.wait_for(lambda: len(collector_rows(platform, receiver_ref, clients)["source"]) >= 24,
                  timeout=40, retry=(RuntimeError, OSError))
    log("GuestBefore", guest=platform.guest(source["ref"], ["/bin/sh", "-ec", GSTATS]).decode(), source=source)
    results = {}
    displacements = {}
    first_fields = None
    captures = []
    for number in (1, 2):
        name = f"child{number}"
        path = f"machines/{machine}"
        accepted = platform.change(path + "/fork-batches", "sequential-fork-" + name, {
            "children": [{"name": name}], "require_independent": True,
        }, platform.wait_capability(path, "state.fork_batch"))
        completed = platform.complete(accepted)
        child, = completed["children"]
        assert completed["effect"] == "applied" and completed["source_lock_released"] and completed["source_disposition"] == "RUNNING", completed
        assert child["state"] == "ready" and child["resource_independence"] == "verified", child
        _, child_ref = platform.context(child["machine_id"])
        candidate = sender(platform, child["machine_id"], child_ref)
        assert candidate["ip"] not in {member["ip"] for member in clients.values()} | {sink}, candidate
        assert all(all(current != previous for current, previous in
                       zip(candidate["guest_identity"], member["guest_identity"], strict=True))
                   for member in clients.values()), candidate
        assert sender(platform, machine, ref) == source, source
        clients[name] = candidate
        fields, region, backings, capture = checkpoint(platform, accepted["operation_id"])
        if first_fields is None:
            first_fields = fields
        else:
            assert fields == first_fields and capture not in captures, capture
        captures.append(capture)
        try:
            sealed = sealed_rings(platform, fields, region, backings)
        except OSError as error:
            sealed = {"unavailable": str(error)}
        log("Published", child=candidate, capture_id=capture, sealed_indices=sealed)
        for label in ("source", name):
            displacements.pop(label, None)
            try:
                displacements[label] = ring_mapping(clients[label], fields, region, backings)
            except (AssertionError, OSError, ValueError) as error:
                log("RingMappingUnavailable", machine=clients[label]["machine"], reason=str(error))
        results[name] = window(platform, receiver_ref, clients, fields, displacements, name)
    assert source == sender(platform, machine, ref)
    _, first_ref = platform.context(clients["child1"]["machine"])
    assert sender(platform, clients["child1"]["machine"], first_ref) == clients["child1"], clients
    log("SequentialFork", bundle_digest=platform.bundle_id, platformd=digest(platform.args.platformd),
        system_image=digest(platform.args.system_image), counts=results, captures=captures, clients=clients)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("OWNED_SEQUENTIAL_FORK_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "OWNED_SEQUENTIAL_FORK_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="owned-sequential-fork-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=8G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        slice_name = f"smolvmownedseq{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        platform = Platform(args, root, stack, (gate.NETWORK,), use_installation=True)
        try:
            platform.start()
            run(platform)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                if (root / name).exists():
                    print(name, (root / name).read_text(errors="replace")[-1800:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
