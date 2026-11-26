#!/usr/bin/env python3
"""Verify public managed Volume operations and exact StatePoint data on KVM."""

import argparse
from contextlib import ExitStack
import errno
import hashlib
import json
import os
from pathlib import Path
import select
import sqlite3
import subprocess
import sys
import tempfile

from managed_system_vsock import Platform, digest, wait_for, worker
from worker_restart_kvm import restart_worker


MIB = 1 << 20


class Product:
    def __init__(self, platform):
        self.platform = platform
        self.sequence = 0

    def operation(self, path, body=None, *, machine=None, revision=None, method="POST"):
        self.sequence += 1
        headers = {"If-Control-Epoch": self.platform.epoch, "Idempotency-Key": f"volume-proof-{self.sequence}"}
        body = {} if body is None else dict(body)
        if machine is not None:
            view = self.platform.app(machine)
            body["expected_generation"] = view["binding_generation"]
            revision = view["revision"]
        if revision is not None:
            headers["If-Match"] = f'"{revision}"'
        accepted = self.platform.app(path, method, body, headers, status=202)
        return wait_for(lambda: (op := self.platform.app("operations/" + accepted["operation_id"]))["state"]
                        in ("succeeded", "failed", "cancelled", "recovery_required") and op, timeout=175)

    def applied(self, *args, **kwargs):
        op = self.operation(*args, **kwargs)
        assert op["state"] == "succeeded" and op["effect"] == "applied", op
        return op

    def failure(self, code, *args, **kwargs):
        op = self.operation(*args, **kwargs)
        assert op["state"] == "failed" and op["effect"] == "none", op
        assert op["error"]["code"].removeprefix("E_") == code, op
        return op

    def machine(self, name):
        op = self.applied("machines", dict(name=name, template_id="t_system", boot_mode="system",
            runtime_instance_id=self.platform.serve_id, start=False, resources={"cpu_count": 1, "memory_mib": 512}))
        return "machines/" + op["machine_id"]

    def volume(self, name, size=8 * MIB):
        return self.applied("volumes", dict(name=name, capacity_bytes=size,
            runtime_instance_id=self.platform.serve_id))["result"]["volume"]

    def read_volume(self, vol):
        return self.platform.app("volumes/" + vol)

    def attach(self, machine, volume, key, **options):
        return self.applied(machine + "/volume-attachments", dict(volume_id=volume["volume_id"],
            expected_volume_revision=volume["revision"], device_key=key, **options),
            machine=machine)["result"]["volume"]["attachments"][0]

    def start(self, machine):
        self.applied(machine + "/start", machine=machine)
        return self.execution(machine)

    def stop(self, machine, ref):
        wait_for(lambda: self.platform.app(machine)["capabilities"]["lifecycle.stop"]["supported"], timeout=40)
        inventory = self.platform.native(f"/instances/{ref['instance_id']}/inventory")
        pidfd = os.pidfd_open(inventory["execution"]["pid"])
        try:
            self.applied(machine + "/stop", machine=machine)
            assert select.select([pidfd], [], [], 4)[0], inventory["execution"]
        finally:
            os.close(pidfd)
        assert self.platform.app(machine)["power_state"] == "stopped"

    def execution(self, machine):
        ref = self.platform.context(machine.split("/")[-1])[1]
        wait_for(lambda: self.platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
                 retry=(RuntimeError, OSError))
        return ref

    def disks(self, ref):
        inventory = self.platform.native(f"/instances/{ref['instance_id']}/inventory")
        blocks = [device["block"] for device in inventory["inventory"]["devices"] if device.get("block")]
        return {block["managed_attachment"]["volume_id"]: (f"/dev/vd{chr(ord('a') + i)}", block["managed_attachment"])
                for i, block in enumerate(blocks)}

    def write(self, ref, path, data):
        self.platform.guest(ref, ["/bin/sh", "-ec",
            'printf %s "$2" | dd of="$1" bs=512 seek=8 conv=notrunc,fsync status=none', "write", path, data])

    def read(self, ref, path, length):
        return self.platform.guest(ref, ["/bin/sh", "-ec",
            'dd if="$1" bs=512 skip=8 count=1 status=none | head -c "$2"', "read", path, str(length)])


def checkpoint_case(platform, *, check_catalog=True, source_disposition="running", verify_retired_head=False):
    product = Product(platform)
    machine = product.machine("Checkpoint disks")
    ids, attachments = [], {}
    for name in ("left", "right"):
        volume = product.volume(name)
        ids.append(volume["volume_id"])
        attachments[volume["volume_id"]] = product.attach(machine, volume, name)
    archive = product.volume("read-only archive")
    archive_id = archive["volume_id"]
    archive = product.applied(f"volumes/{archive_id}/snapshots", revision=archive["revision"])["result"]["volume"]
    archived = archive["snapshots"][0]
    archive_lease = product.attach(machine, archive, "archive", mode="ro", snapshot_id=archived["snapshot_id"])
    attachments[archive_id] = archive_lease
    ref = product.start(machine)
    originals = product.disks(ref)
    first = ["checkpoint-left-abcdef", "checkpoint-right-0123"]
    later = ["live-latest-left-aaaa", "live-latest-right-bbbb"]
    for ident, value in zip(ids, first, strict=True):
        product.write(ref, originals[ident][0], value)
    platform.guest(ref, ["/bin/sh", "-ec", "printf checkpoint-ram >/run/volume-proof"])
    checkpoint = product.applied(machine + "/snapshots", {"label": "Before independent writes", "capture_mode": "vm"},
                                 machine=machine)["result"]
    assert checkpoint["statepoint"]["phase"] == "durable", checkpoint
    snapshot = checkpoint["snapshot_id"]
    for ident, value in zip(ids, later, strict=True):
        product.write(ref, originals[ident][0], value)
        assert product.read(ref, originals[ident][0], len(value)) == value.encode()
    platform.guest(ref, ["/bin/sh", "-ec", "printf current-ram >/run/volume-proof"])
    advanced = product.disks(ref)
    assert all(advanced[ident][1]["writable_head_id"] != originals[ident][1]["writable_head_id"] for ident in ids)
    print(f"Checkpoint {snapshot} holds {first}; the live source advanced its two volume heads to {later}.", flush=True)

    current_ref = ref
    active_archive = archive_lease["attachment_id"]
    if source_disposition != "running":
        product.stop(machine, ref)
        moved = product.machine("Original volumes moved elsewhere")
        for volume, attachment in attachments.items():
            original = product.read_volume(volume)
            product.applied("volume-attachments/" + attachment["attachment_id"],
                            {"expected_volume_revision": original["revision"]}, machine=machine, method="DELETE")
            assert product.read_volume(volume)["attachments"] == []
            options = {"mode": "ro", "snapshot_id": archived["snapshot_id"]} if volume == archive_id else {}
            next_lease = product.attach(moved, product.read_volume(volume), attachment["device_key"], **options)
            assert next_lease["attachment_id"] != attachment["attachment_id"]
            if volume == archive_id:
                active_archive = next_lease["attachment_id"]
        assert platform.app(machine + "/volume-attachments")["items"] == []
        if source_disposition == "deleted":
            product.applied(machine, machine=machine, method="DELETE")
            if verify_retired_head:
                root_volume, = set(advanced) - set(attachments)
                latest_head = advanced[root_volume][1]["writable_head_id"]
                captured_head = originals[root_volume][1]["writable_head_id"]
                retired = worker(platform.worker, "QueryResource", {"resource_id": latest_head})
                assert retired["state"] == "deleted" and not (platform.root / "catalog/objects" / latest_head).exists()
                assert worker(platform.worker, "QueryResource", {"resource_id": captured_head})["state"] == "available"
                print(f"Source private head {latest_head} was removed; checkpoint head {captured_head} remained pinned.",
                      flush=True)
        current_ref = product.start(moved)
        advanced = product.disks(current_ref)
        for volume, value in zip(ids, later, strict=True):
            assert product.read(current_ref, advanced[volume][0], len(value)) == value.encode()
        print(f"Original product attachments were detached and the same volumes moved with latest bytes. "
              f"The capturing compute is {source_disposition}.", flush=True)

    exact = product.failure("VOLUME_GENERATION_MISMATCH", "restores",
                            {"name": "Exact conflict", "snapshot_id": snapshot, "volume_restore_policy": "exact"})
    print(f"Exact public restore returned {exact['error']['code']} and effect none.", flush=True)
    if source_disposition == "running":
        assert platform.context(machine.split("/")[-1])[1] == ref
    cloned = product.applied("restores", {"name": "Saved disk clone", "snapshot_id": snapshot,
                                         "volume_restore_policy": "clone_conflicts"})
    clone = "machines/" + cloned["machine_id"]
    restored = product.execution(clone)
    assert restored["instance_id"] not in {ref["instance_id"], current_ref["instance_id"]} and restored["execution_epoch"] == 1
    receipt = cloned["result"]
    assert receipt["statepoint"]["manifest_digest"] == checkpoint["statepoint"]["manifest_digest"]
    mapping = receipt["volume_map"]
    clone_disks = product.disks(restored)
    assert len({mapping[ident] for ident in ids}) == len(ids) and all(mapping[ident] != ident for ident in ids)
    assert mapping[archive_id] == archive_id
    assert clone_disks[archive_id][1]["generation_id"] == originals[archive_id][1]["generation_id"] == archived["generation_id"]
    assert platform.guest(restored, ["/usr/sbin/blockdev", "--getro", clone_disks[archive_id][0]]) == b"1\n"
    assert platform.guest(restored, ["/bin/cat", "/run/volume-proof"]) == b"checkpoint-ram"
    for ident, old, current in zip(ids, first, later, strict=True):
        target, disk = clone_disks[mapping[ident]]
        assert disk["writable_head_id"] != advanced[ident][1]["writable_head_id"]
        assert product.read(restored, target, len(old)) == old.encode()
        assert product.read(current_ref, advanced[ident][0], len(current)) == current.encode()
        product.write(restored, target, "clone-only")
        assert product.read(current_ref, advanced[ident][0], len(current)) == current.encode()
    print(f"clone_conflicts restored saved RAM and both exact bytes on new volumes {[(v, mapping[v]) for v in ids]}. "
          "Writes in the clone did not change the independently advanced source.", flush=True)
    if check_catalog:
        platform.restart()
        projected = platform.app(clone + "/volume-attachments")["items"]
        expected = {mapping[ident] for ident in ids} | {archive_id}
        assert {item["volume_id"] for item in projected} == expected, projected
        for ident in ids:
            public = product.read_volume(mapping[ident])
            assert public["head_resource_id"] == clone_disks[mapping[ident]][1]["writable_head_id"], public
            assert len(public["attachments"]) == 1 and public["attachments"][0]["state"] == "attached", public
        shared = product.read_volume(archive_id)["attachments"]
        assert len(shared) == 2 and all(item["state"] == "attached" and item["generation_id"] == archived["generation_id"]
                                        and item["mode"] == "ro" for item in shared), shared
        assert active_archive in {item["attachment_id"] for item in shared}
        assert {item["instance_id"] for item in shared} == {current_ref["instance_id"], restored["instance_id"]}
        print(f"Each new writable volume is public. Both KVMs hold separate read-only leases on {archived['generation_id']}.",
              flush=True)
        product.applied(clone + "/hibernate", machine=clone)
        assert platform.app(clone)["power_state"] == "hibernated"
        for item in projected:
            volume = product.read_volume(item["volume_id"])
            product.failure("VOLUME_TOPOLOGY_UNAVAILABLE", "volume-attachments/" + item["attachment_id"],
                            dict(expected_volume_revision=volume["revision"]), machine=clone, method="DELETE")
        resumed = product.start(clone)
        assert resumed["execution_epoch"] == restored["execution_epoch"] + 1
        disks = product.disks(resumed)
        for ident in ids:
            assert product.read(resumed, disks[mapping[ident]][0], len("clone-only")) == b"clone-only"
        assert {item["volume_id"] for item in platform.app(clone + "/volume-attachments")["items"]} == expected
        print("Hibernated topology stayed immutable. Exact wake retained the projected volume IDs and clone writes.", flush=True)


def lifecycle_case(platform):
    product = Product(platform)
    machine = product.machine("Volume lifecycle source")
    filesystem, data, spare = (product.volume(name, 16 * MIB) for name in ("filesystem", "portable data", "spare"))
    fsid, vid, spareid = (volume["volume_id"] for volume in (filesystem, data, spare))
    product.attach(machine, filesystem, "filesystem")
    attachment = product.attach(machine, data, "portable")
    first = product.start(machine)
    disks = product.disks(first)
    fs, raw = (disks[ident][0] for ident in (fsid, vid))
    marker = b"fixture-volume-file"
    raw_marker = "portable-original-bytes"
    platform.guest(first, ["/usr/sbin/mke2fs", "-q", "-F", "-t", "ext4", fs])
    platform.guest(first, ["/bin/sh", "-ec",
        'mkdir -p /mnt/volume-proof; mount "$1" /mnt/volume-proof; printf %s "$2" >/mnt/volume-proof/proof; sync; umount /mnt/volume-proof',
        "filesystem", fs, marker.decode()])
    product.write(first, raw, raw_marker)

    def block_size(ref, disk):
        return int(platform.guest(ref, ["/usr/sbin/blockdev", "--getsize64", disk]))

    def filesystem_size(ref, disk):
        stats = platform.guest(ref, ["/usr/sbin/tune2fs", "-l", disk]).decode()
        rows = dict(line.split(":", 1) for line in stats.splitlines() if ":" in line)
        return int(rows["Block size"]) * int(rows["Block count"])

    def file(ref, disk):
        return platform.guest(ref, ["/bin/sh", "-ec",
            'mkdir -p /mnt/volume-proof; mount "$1" /mnt/volume-proof; cat /mnt/volume-proof/proof; umount /mnt/volume-proof',
            "read-filesystem", disk])

    def grow(ref_volume):
        return dict(path="volumes/" + fsid, body=dict(capacity_bytes=32 * MIB),
                    revision=ref_volume["revision"], method="PATCH")

    def detach(volume):
        return dict(path="volume-attachments/" + attachment["attachment_id"],
                    body=dict(expected_volume_revision=volume["revision"]), machine=machine, method="DELETE")

    before_fs = filesystem_size(first, fs)
    assert block_size(first, fs) == 16 * MIB and file(first, fs) == marker
    product.failure("CONFLICT", **grow(product.read_volume(fsid)))
    product.failure("PRECONDITION_FAILED", **detach(product.read_volume(vid)))
    product.failure("PRECONDITION_FAILED", machine + "/volume-attachments",
                    dict(volume_id=spareid, expected_volume_revision=spare["revision"], device_key="spare", mode="rw"),
                    machine=machine)
    assert block_size(first, fs) == 16 * MIB and filesystem_size(first, fs) == before_fs
    assert product.read(first, raw, len(raw_marker)) == raw_marker.encode()
    assert set(product.disks(first)) == set(disks)
    print("Live attachment changes required a fenced stopped execution; live growth returned CONFLICT. "
          "The Guest devices, capacity and bytes stayed intact.", flush=True)

    product.stop(machine, first)
    assert all(item["state"] == "reserved" for item in platform.app(machine + "/volume-attachments")["items"])
    grown = product.applied(**grow(product.read_volume(fsid)))["result"]
    assert grown["block_capacity_applied"] and grown["filesystem_resize_status"] == "not_requested", grown
    assert grown["volume"]["capacity_bytes"] == 32 * MIB, grown
    product.applied(**detach(product.read_volume(vid)))
    assert {item["volume_id"] for item in platform.app(machine + "/volume-attachments")["items"]} == {fsid}
    assert product.read_volume(vid)["attachments"] == []
    assert product.read_volume(vid)["lifecycle"] == "persistent"

    second = product.start(machine)
    assert second["execution_epoch"] == first["execution_epoch"] + 1
    devices = product.disks(second)
    assert fsid in devices and vid not in devices and len(devices) == len(disks) - 1, devices
    fs = devices[fsid][0]
    assert block_size(second, fs) == 32 * MIB
    assert filesystem_size(second, fs) == before_fs and file(second, fs) == marker
    platform.guest(second, ["/bin/sh", "-c",
        '/usr/sbin/e2fsck -fp "$1"; test "$?" -le 1', "check-before-growth", fs])
    platform.guest(second, ["/usr/sbin/resize2fs", fs])
    assert filesystem_size(second, fs) == 32 * MIB and file(second, fs) == marker

    target = product.machine("Volume lifecycle destination")
    product.attach(target, product.read_volume(vid), "imported-data")
    moved = product.start(target)
    target_disk = product.disks(moved)[vid][0]
    assert product.read(moved, target_disk, len(raw_marker)) == raw_marker.encode()
    print(f"Public Stop, grow and detach moved persistent {vid} to a different KVM with its exact bytes. "
          f"{fsid} booted with a 32 MiB block device, a {before_fs}-byte filesystem and its original file. "
          "The Guest then expanded ext4 to 32 MiB.", flush=True)


class FaultPlatform(Platform):
    """Load the precise fsync fault only in this fixture's Worker process."""

    def spawn(self, command, log):
        if Path(command[0]).name != "smolvm-resourced":
            return super().spawn(command, log)
        prior = self.env
        self.env = {**prior, "LD_PRELOAD": str(self.args.fsync_shim),
                    "VOLUME_TEST_SEAL_FAULT": str(self.root / "seal-fault")}
        try:
            return super().spawn(command, log)
        finally:
            self.env = prior


def isolate_objects(root, stack):
    image = root / "objects.ext4"
    with image.open("wb") as target:
        target.truncate(1350 * MIB)
    subprocess.run(["mkfs.ext4", "-q", "-F", "-m", "0", image], check=True)
    objects = root / "catalog/objects"
    objects.parent.mkdir(mode=0o700)
    objects.mkdir(mode=0o700)
    subprocess.run(["mount", "-o", "loop", image, objects], check=True)
    stack.callback(lambda: wait_for(lambda: subprocess.run(
        ["umount", objects], capture_output=True).returncode == 0, timeout=10))
    objects.chmod(0o700)
    return objects


def fill(objects, leave=0):
    file = objects / "proof-filler"
    with file.open("ab", buffering=0) as out:
        stats = os.statvfs(objects)
        free = stats.f_bavail * stats.f_frsize
        if leave:
            assert free > leave
            os.posix_fallocate(out.fileno(), os.fstat(out.fileno()).st_size, free - leave)
        else:
            block = b"x" * stats.f_frsize
            try:
                for _ in range(free // len(block) + 1):
                    out.write(block)
            except OSError as error:
                assert error.errno == errno.ENOSPC, error
            else:
                raise AssertionError("isolated object disk did not reach ENOSPC")
        os.fsync(out.fileno())
    return file


def growth_fault_case(platform, objects):
    product = Product(platform)
    machine = product.machine("Volume fault recovery")
    initial = product.volume("grow under real ENOSPC", 16 * MIB)
    ident = initial["volume_id"]
    product.attach(machine, initial, "fault-data")
    first = product.start(machine)
    disk = product.disks(first)[ident][0]
    platform.guest(first, ["/bin/sh", "-ec",
        'yes "scope verified volume" | head -c 16777216 | dd of="$1" bs=1M conv=notrunc,fsync status=none', "seed", disk])

    def guest_hash(execution, path):
        return platform.guest(execution, ["/bin/sh", "-ec",
            'dd if="$1" bs=1M count=16 iflag=fullblock status=none | sha256sum', "hash", path]).split()[0].decode()

    expected = guest_hash(first, disk)
    product.stop(machine, first)
    original = product.read_volume(ident)
    head = original["head_resource_id"]
    old_file = objects / head / "data"
    old_inode = old_file.stat().st_ino
    assert digest(old_file) == "sha256:" + expected
    grow = dict(path=f"volumes/{ident}", method="PATCH", revision=original["revision"], body={"capacity_bytes": 32 * MIB})
    catalog = f"file:{platform.root}/catalog/catalog.sqlite?mode=ro"

    def row(sql, *params):
        with sqlite3.connect(catalog, uri=True) as database:
            return database.execute(sql, params).fetchone()

    def clean_abort(op):
        ident = op["operation_id"]
        phase, saved = row("SELECT phase,result_json FROM operations JOIN product_volume_operations USING(operation_id) "
                           "WHERE operation_id=?", ident)
        assert phase == "ABORTED" and "no space" in json.loads(saved)["storage_not_applied"].lower(), (phase, saved)
        assert not row("SELECT resource_id FROM managed_blocks WHERE operation_id=?", ident)
        assert not (objects / (ident + "-head")).exists()

    filler = fill(objects, leave=2 * MIB)
    blocked = product.operation(**grow)
    if blocked["state"] != "failed" or blocked["effect"] != "none":
        ident = blocked["operation_id"]
        mode = oct(old_file.stat().st_mode & 0o777)
        pending = row("SELECT phase,result_json FROM operations JOIN product_volume_operations USING(operation_id) "
                      "WHERE operation_id=?", ident)
        seal = row("SELECT state FROM root_refs WHERE owner_kind='block_seal' AND owner_id=?", ident)
        disk = os.statvfs(objects)
        print(f"Failed growth metadata {pending}, seal {seal}, old mode {mode}, "
              f"free bytes {disk.f_bavail * disk.f_frsize}, staging paths {[p.name for p in objects.glob('.block-*')]}.",
              flush=True)
    assert blocked["state"] == "failed" and blocked["effect"] == "none" and (
        blocked["error"]["code"] == "VOLUME_STORAGE_NOT_APPLIED"), blocked
    clean_abort(blocked)
    assert product.read_volume(ident) == original
    assert old_file.stat().st_mode & 0o777 == 0o600 and old_file.stat().st_ino == old_inode
    assert digest(old_file) == "sha256:" + expected
    fill(objects)
    refused_create = product.failure("VOLUME_STORAGE_NOT_APPLIED", "volumes", {
        "name": "unapplied head", "capacity_bytes": 4 * MIB, "runtime_instance_id": platform.serve_id,
    })
    clean_abort(refused_create)
    assert len(platform.app("volumes")["items"]) == 1
    filler.unlink()
    reopened = product.start(machine)
    assert guest_hash(reopened, product.disks(reopened)[ident][0]) == expected
    product.stop(machine, reopened)
    print(f"Actual ext4 ENOSPC aborted create and grow without a published head. "
          f"The original inode {old_inode}, revision and 16 MiB data sha256:{expected} survived a cold boot.", flush=True)

    selector = platform.root / "seal-fault"
    selector.write_text(str(old_file))
    interrupted = product.operation(**grow)
    assert interrupted["state"] == "recovery_required" and interrupted["effect"] == "unknown", interrupted
    assert selector.with_suffix(".observed").is_file()
    key = interrupted["operation_id"]
    assert old_file.stat().st_mode & 0o777 == 0o400
    assert row("SELECT state FROM root_refs WHERE owner_kind='block_seal' AND owner_id=? AND resource_id=?", key, head) == ("active",)
    prepared = objects / (key + "-head") / "data"
    staged_inode = prepared.stat().st_ino
    assert prepared.stat().st_size == 32 * MIB
    with prepared.open("rb") as source:
        assert hashlib.sha256(source.read(16 * MIB)).hexdigest() == expected
    assert product.read_volume(ident)["state"] == "changing"
    runtime_name = platform.app(machine)["runtime_name"]
    assert platform.native(f"/machines/{runtime_name}/binding")["incarnation_id"] is None
    rival = product.failure("CONFLICT", f"volumes/{ident}/snapshots", revision=original["revision"])
    assert not row("SELECT operation_id FROM product_volume_operations WHERE operation_id=?", rival["operation_id"])
    print(f"Scoped fsync EIO followed the original head's mode change to 0400. "
          f"Rollback is fenced and the real replacement inode {staged_inode} is durable for reconciliation.", flush=True)

    health = worker(platform.worker, "Health")
    binary = platform.bundle / "smolvm-resourced"
    restart_worker(platform, binary)
    def replacement():
        current = worker(platform.worker, "Health")
        return current if current["broker_session_id"] != health["broker_session_id"] else None

    current = wait_for(replacement, retry=(OSError, RuntimeError))
    assert current["state"] == "recovering" and current["executable_sha256"] == digest(binary), current
    selector.unlink()
    ack = platform.app(f"operations/{key}/reconcile", "POST", {}, {
        "If-Control-Epoch": platform.epoch, "Idempotency-Key": "repair-volume-seal",
    })
    settled = platform.complete(ack)
    volume = settled["result"]["volume"]
    assert volume["head_resource_id"] == key + "-head" and volume["revision"] == original["revision"] + 1
    assert prepared.stat().st_ino == staged_inode and volume["capacity_bytes"] == 32 * MIB
    assert row("SELECT state FROM root_refs WHERE owner_kind='block_seal' AND owner_id=? AND resource_id=?", key, head) == ("dropped",)
    # An empty Worker has no live VMM to complete its boot recovery for it.
    if worker(platform.worker, "Health")["state"] == "recovering":
        assert worker(platform.worker, "CompleteRecovery", {"broker_session_id": current["broker_session_id"]})["state"] == "ready"
    execution = product.start(machine)
    new_disk = product.disks(execution)[ident][0]
    assert guest_hash(execution, new_disk) == expected
    assert platform.guest(execution, ["/usr/sbin/blockdev", "--getsize64", new_disk]) == b"33554432\n"
    product.write(execution, new_disk, "active-after-reconcile")
    assert product.read(execution, new_disk, len("active-after-reconcile")) == b"active-after-reconcile"
    print("After Worker SIGKILL, public reconcile completed the original operation and reused the staged inode. "
          "A fresh KVM read the original 16 MiB data and wrote to the 32 MiB successor.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--checkpoint-data-only", action="store_true",
                        help="omit projection assertions when measuring a historical installation")
    parser.add_argument("--case", choices=("checkpoint", "detached-checkpoint", "deleted-checkpoint", "lifecycle", "growth-fault"),
                        default="checkpoint")
    parser.add_argument("--fsync-shim", type=lambda path: Path(path).resolve(strict=True),
                        help="prebuilt shared library from tests/fixtures/volume-fsync-fault.c")
    parser.add_argument("--verify-retired-head", action="store_true",
                        help="check physical private-head deletion in the deleted-checkpoint case")
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root, KVM, and systemd")
    if os.environ.get("MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE") != "1":
        os.execvp("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--", "/usr/bin/env",
                              "MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE=1", "PYTHONDONTWRITEBYTECODE=1",
                              sys.executable, __file__, *sys.argv[1:]])
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="volume-proof-") as tmp, ExitStack() as stack:
        root = Path(tmp)
        root.chmod(0o755)
        unit = f"smolvmvolume{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        if args.case == "growth-fault":
            if args.fsync_shim is None:
                args.fsync_shim = root / "volume-fsync-fault.so"
                source = Path(__file__).resolve().parents[1] / "fixtures/volume-fsync-fault.c"
                subprocess.run(["cc", "-shared", "-fPIC", "-O2", "-o", args.fsync_shim, source, "-ldl"], check=True)
            objects = isolate_objects(root, stack)
            platform = FaultPlatform(args, root, stack)
        else:
            platform = Platform(args, root, stack)
        try:
            platform.start()
            manifest = json.loads((args.bundle / "runtime-bundle.json").read_text())
            expected = next(entry["sha256"] for entry in manifest["artifacts"] if entry["path"] == "smolvm-resourced")
            assert platform.bundle_id == manifest["bundle_digest"]
            assert worker(platform.worker, "Health")["executable_sha256"] == "sha256:" + expected
            print(f"Live Bundle {platform.bundle_id}, Worker sha256:{expected}, "
                  f"platformd {digest(args.platformd)}.", flush=True)
            if args.case.endswith("checkpoint"):
                source_disposition = {"checkpoint": "running", "detached-checkpoint": "stopped", "deleted-checkpoint": "deleted"}[args.case]
                checkpoint_case(platform, check_catalog=not args.checkpoint_data_only, source_disposition=source_disposition,
                                verify_retired_head=args.verify_retired_head)
            elif args.case == "lifecycle":
                lifecycle_case(platform)
            else:
                growth_fault_case(platform, objects)
        except BaseException:
            for log in [root / "platform.log", root / "worker.log", root / "serve.log",
                        *(root / "state/managed").glob("*.log")]:
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-4500:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
