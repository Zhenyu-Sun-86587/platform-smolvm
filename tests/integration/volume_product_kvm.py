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

    def volume(self, name, size=8 * MIB, *, lifecycle=None):
        body = dict(name=name, capacity_bytes=size, runtime_instance_id=self.platform.serve_id)
        if lifecycle is not None:
            body["lifecycle"] = lifecycle
        return self.applied("volumes", body)["result"]["volume"]

    def read_volume(self, vol):
        return self.platform.app("volumes/" + vol)

    def attach(self, machine, volume, key, **options):
        op = self.applied(machine + "/volume-attachments", dict(volume_id=volume["volume_id"],
            expected_volume_revision=volume["revision"], device_key=key, **options),
            machine=machine)
        ident = "vatt_" + op["operation_id"].removeprefix("op_")
        return next(item for item in op["result"]["volume"]["attachments"] if item["attachment_id"] == ident)

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

    def head_collected(self, resource):
        return worker(self.platform.worker, "QueryResource", {"resource_id": resource})["state"] == "deleted" and (
            not (self.platform.root / "catalog/objects" / resource).exists())

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
    private = None
    for name in ("left", "right"):
        machine_lifetime = platform.args.machine_volumes and name == "left" and source_disposition == "running"
        volume = product.volume(name, lifecycle="machine" if machine_lifetime else None)
        if machine_lifetime:
            private = volume["volume_id"]
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
        assert disk["generation_id"] == originals[ident][1]["writable_head_id"], disk
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
        assert {item["volume_id"]: item["attachment_id"] for item in projected} == (
            receipt["statepoint"]["product_attachments"]), (projected, receipt)
        for ident in ids:
            public = product.read_volume(mapping[ident])
            assert public["head_resource_id"] == clone_disks[mapping[ident]][1]["writable_head_id"], public
            assert len(public["attachments"]) == 1 and public["attachments"][0]["state"] == "attached", public
            if ident == private:
                assert public["lifecycle"] == "machine" and public["machine_instance_id"] == restored["instance_id"], public
        shared = product.read_volume(archive_id)["attachments"]
        assert len(shared) == 2 and all(item["state"] == "attached" and item["generation_id"] == archived["generation_id"]
                                        and item["mode"] == "ro" for item in shared), shared
        assert active_archive in {item["attachment_id"] for item in shared}
        assert {item["instance_id"] for item in shared} == {current_ref["instance_id"], restored["instance_id"]}
        print(f"Each new writable volume is public. Both KVMs hold separate read-only leases on {archived['generation_id']}.",
              flush=True)
        product.applied(clone + "/hibernate", machine=clone)
        assert platform.app(clone)["power_state"] == "hibernated"
        hibernated_refusals = []
        for item in projected:
            volume = product.read_volume(item["volume_id"])
            failure = product.failure("VOLUME_TOPOLOGY_UNAVAILABLE", "volume-attachments/" + item["attachment_id"],
                                      dict(expected_volume_revision=volume["revision"]), machine=clone, method="DELETE")
            if platform.args.machine_volumes:
                assert failure["error"]["reason_code"] == "REQUIRES_STOPPED", failure
            hibernated_refusals.append(failure["error"])
        print(json.dumps({"hibernated_topology_refusals": hibernated_refusals}, sort_keys=True), flush=True)
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
    ephemeral = product.volume("legacy ephemeral data", lifecycle="ephemeral")
    archive = product.volume("immutable archive")
    ephemeral_id, archive_id = (volume["volume_id"] for volume in (ephemeral, archive))
    product.attach(machine, filesystem, "filesystem")
    attachment = product.attach(machine, data, "portable")
    product.attach(machine, ephemeral, "ephemeral")
    archive_writer = product.attach(machine, archive, "archive")
    private = None
    if platform.args.machine_volumes:
        private = product.volume("instance-owned writable data", lifecycle="machine")
        assert private["lifecycle"] == "machine" and "machine_instance_id" not in private
        private_id = private["volume_id"]
        private_lease = product.attach(machine, private, "private")
        assert product.read_volume(private_id)["machine_instance_id"] == private_lease["instance_id"]
    first = product.start(machine)
    disks = product.disks(first)
    fs, raw = (disks[ident][0] for ident in (fsid, vid))
    marker = b"fixture-volume-file"
    raw_marker = "portable-original-bytes"
    ephemeral_marker, archive_marker = "legacy-ephemeral-bytes", "sealed-archive-bytes"
    platform.guest(first, ["/usr/sbin/mke2fs", "-q", "-F", "-t", "ext4", fs])
    platform.guest(first, ["/bin/sh", "-ec",
        'mkdir -p /mnt/volume-proof; mount "$1" /mnt/volume-proof; printf %s "$2" >/mnt/volume-proof/proof; sync; umount /mnt/volume-proof',
        "filesystem", fs, marker.decode()])
    product.write(first, raw, raw_marker)
    product.write(first, disks[ephemeral_id][0], ephemeral_marker)
    product.write(first, disks[archive_id][0], archive_marker)
    if private is not None:
        product.write(first, disks[private_id][0], "machine-private-bytes")

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

    def detach(volume, lease):
        return dict(path="volume-attachments/" + lease["attachment_id"],
                    body=dict(expected_volume_revision=volume["revision"]), machine=machine, method="DELETE")

    def check_readonly(ref, device, text=archive_marker):
        assert platform.guest(ref, ["/usr/sbin/blockdev", "--getro", device]) == b"1\n"
        assert product.read(ref, device, len(text)) == text.encode()

    before_fs = filesystem_size(first, fs)
    assert block_size(first, fs) == 16 * MIB and file(first, fs) == marker
    live_grow = product.failure("CONFLICT", **grow(product.read_volume(fsid)))
    live_detach = product.failure("PRECONDITION_FAILED", **detach(product.read_volume(vid), attachment))
    live_attach = product.failure("PRECONDITION_FAILED", machine + "/volume-attachments",
                    dict(volume_id=spareid, expected_volume_revision=spare["revision"], device_key="spare", mode="rw"),
                    machine=machine)
    failures = {name: op["error"] for name, op in (("grow", live_grow), ("detach", live_detach), ("attach", live_attach))}
    if platform.args.machine_volumes:
        assert all(error["reason_code"] == "REQUIRES_STOPPED" for error in failures.values()), failures
    capabilities = platform.app(machine)["capabilities"]
    assert not any(capabilities.get(name, {}).get("supported") for name in
                   ("storage.block_hotplug", "storage.block_hotunplug"))
    assert block_size(first, fs) == 16 * MIB and filesystem_size(first, fs) == before_fs
    assert product.read(first, raw, len(raw_marker)) == raw_marker.encode()
    assert set(product.disks(first)) == set(disks)
    print(json.dumps({"live_volume_preconditions": failures, "guest_devices_unchanged": sorted(disks)}, sort_keys=True),
          flush=True)

    product.stop(machine, first)
    assert all(item["state"] == "reserved" for item in platform.app(machine + "/volume-attachments")["items"])
    grown = product.applied(**grow(product.read_volume(fsid)))["result"]
    assert grown["block_capacity_applied"] and grown["filesystem_resize_status"] == "not_requested", grown
    assert grown["volume"]["capacity_bytes"] == 32 * MIB, grown
    product.applied(**detach(product.read_volume(vid), attachment))
    product.applied(**detach(product.read_volume(archive_id), archive_writer))
    archive = product.read_volume(archive_id)
    archive = product.applied(f"volumes/{archive_id}/snapshots", revision=archive["revision"])["result"]["volume"]
    archived, = archive["snapshots"]
    readonly = product.attach(machine, archive, "archive", mode="ro", snapshot_id=archived["snapshot_id"])
    wanted = {fsid, ephemeral_id, archive_id}
    if private is not None:
        owned = product.read_volume(private_id)
        private_archive = product.applied(f"volumes/{private_id}/snapshots",
                                          revision=owned["revision"])["result"]["volume"]
        private_snapshot, = private_archive["snapshots"]
        wanted.add(private_id)
    assert {item["volume_id"] for item in platform.app(machine + "/volume-attachments")["items"]} == wanted
    assert product.read_volume(vid)["attachments"] == []
    assert product.read_volume(vid)["lifecycle"] == "persistent"

    second = product.start(machine)
    assert second["execution_epoch"] == first["execution_epoch"] + 1
    devices = product.disks(second)
    root_volume, = set(disks) - wanted - {vid}
    assert set(devices) == wanted | {root_volume}, devices
    fs = devices[fsid][0]
    assert block_size(second, fs) == 32 * MIB
    assert filesystem_size(second, fs) == before_fs and file(second, fs) == marker
    platform.guest(second, ["/bin/sh", "-c",
        '/usr/sbin/e2fsck -fp "$1"; test "$?" -le 1', "check-before-growth", fs])
    platform.guest(second, ["/usr/sbin/blockdev", "--setro", fs])
    platform.guest(second, ["/bin/sh", "-ec",
        'if /usr/sbin/resize2fs "$1"; then exit 1; fi', "expected-filesystem-failure", fs])
    assert block_size(second, fs) == 32 * MIB and filesystem_size(second, fs) == before_fs
    assert product.read_volume(fsid)["capacity_bytes"] == 32 * MIB
    platform.guest(second, ["/usr/sbin/blockdev", "--setrw", fs])
    platform.guest(second, ["/usr/sbin/resize2fs", fs])
    assert filesystem_size(second, fs) == 32 * MIB and file(second, fs) == marker
    assert product.read(second, devices[ephemeral_id][0], len(ephemeral_marker)) == ephemeral_marker.encode()
    check_readonly(second, devices[archive_id][0])
    assert devices[archive_id][1]["generation_id"] == archived["generation_id"]
    refused_readonly = product.failure("PRECONDITION_FAILED", **detach(product.read_volume(archive_id), readonly))
    if platform.args.machine_volumes:
        assert refused_readonly["error"]["reason_code"] == "REQUIRES_STOPPED", refused_readonly
    if private is not None:
        assert product.read(second, devices[private_id][0], len("machine-private-bytes")) == b"machine-private-bytes"

    target = product.machine("Volume lifecycle destination")
    product.attach(target, product.read_volume(vid), "imported-data")
    product.attach(target, product.read_volume(archive_id), "shared-archive",
                   mode="ro", snapshot_id=archived["snapshot_id"])
    if private is not None:
        target_lease = product.attach(target, product.read_volume(private_id), "machine-ro",
            mode="ro", snapshot_id=private_snapshot["snapshot_id"])
        assert product.read_volume(private_id)["machine_instance_id"] == first["instance_id"]
    moved = product.start(target)
    destination = product.disks(moved)
    assert product.read(moved, destination[vid][0], len(raw_marker)) == raw_marker.encode()
    check_readonly(moved, destination[archive_id][0])
    assert destination[archive_id][1]["generation_id"] == archived["generation_id"]
    shared = product.read_volume(archive_id)["attachments"]
    assert len(shared) == 2 and all(lease["state"] == "attached" and lease["mode"] == "ro" for lease in shared)

    product.stop(machine, second)
    # A detached machine volume remains owned and retirement must still reclaim it.
    if private is not None:
        product.applied(**detach(product.read_volume(private_id), private_lease))
        other = product.machine("Retained snapshot reader")
        current = product.read_volume(private_id)
        assert current["machine_instance_id"] == first["instance_id"]
        other_lease = product.attach(other, current, "retained-machine-ro",
            mode="ro", snapshot_id=private_snapshot["snapshot_id"])
    product.applied(machine, machine=machine, method="DELETE")
    assert product.read_volume(fsid)["attachments"] == []
    detached_ephemeral = product.read_volume(ephemeral_id)
    assert detached_ephemeral["lifecycle"] == "ephemeral" and detached_ephemeral["attachments"] == []
    root_head = devices[root_volume][1]["writable_head_id"]

    wait_for(lambda: product.head_collected(root_head))
    check_readonly(moved, destination[archive_id][0])
    if private is not None:
        private_head = devices[private_id][1]["writable_head_id"]
        wait_for(lambda: product.head_collected(private_head))
        assert private_id not in {item["volume_id"] for item in platform.app("volumes")["items"]}
        platform.app("volumes/" + private_id, status=404)
        print(f"Machine volume {private_id} kept its owner after detach and physically collected head {private_head} "
              "with the source. Public Volume list and read no longer expose it.", flush=True)
        assert destination[private_id][1]["generation_id"] == private_snapshot["generation_id"]
        check_readonly(moved, destination[private_id][0], "machine-private-bytes")
        other_ref = product.start(other)
        device, binding = product.disks(other_ref)[private_id]
        assert binding["generation_id"] == private_snapshot["generation_id"]
        check_readonly(other_ref, device, "machine-private-bytes")
        public_snapshot = platform.app("volume-snapshots/" + private_snapshot["snapshot_id"])
        product.stop(target, moved)
        detached = product.applied("volume-attachments/" + target_lease["attachment_id"],
                                 {"expected_volume_revision": public_snapshot["volume_revision"]},
                                 machine=target, method="DELETE")["result"]["volume"]
        assert detached["state"] == "deleted" and detached["machine_instance_id"] == first["instance_id"]
        assert {lease["attachment_id"] for lease in detached["attachments"]} == {other_lease["attachment_id"]}
        check_readonly(other_ref, device, "machine-private-bytes")
        assert (platform.root / "catalog/objects" / private_snapshot["generation_id"] / "data").is_file()
        print(f"Two independent RO consumers used {private_snapshot['generation_id']} after owner deletion. "
              "The second cold-booted from the retired volume's immutable snapshot and kept reading after the first detached.",
              flush=True)
    print(f"Public Stop, grow and detach moved persistent {vid} to a different KVM with its exact bytes. "
          f"{fsid} booted with a 32 MiB block device and a {before_fs}-byte filesystem. "
          "A read-only Guest filesystem resize failed, leaving the exposed block size intact. "
          "The Guest then expanded ext4 to 32 MiB and read the original file. "
          f"Legacy ephemeral {ephemeral_id} survived Delete. Private root {root_head} was collected. "
          f"The immutable archive {archived['generation_id']} remained readable in the other KVM.", flush=True)


def machine_fork_case(platform):
    product = Product(platform)
    parent = product.machine("Machine-owned Fork parent")
    private = product.volume("Private Fork data", lifecycle="machine")
    persistent = product.volume("Persistent Fork data")
    archive = product.volume("Fork archive")
    product.attach(parent, private, "fork-data")
    product.attach(parent, persistent, "fork-persistent")
    archive_writer = product.attach(parent, archive, "fork-archive")
    archive_id = archive["volume_id"]
    first = product.start(parent)
    archive_mark = "fork-immutable-data"
    product.write(first, product.disks(first)[archive_id][0], archive_mark)
    product.stop(parent, first)
    archive = product.read_volume(archive_id)
    product.applied("volume-attachments/" + archive_writer["attachment_id"],
                    {"expected_volume_revision": archive["revision"]}, machine=parent, method="DELETE")
    archive = product.read_volume(archive_id)
    archive = product.applied(f"volumes/{archive_id}/snapshots", revision=archive["revision"])["result"]["volume"]
    snapshot, = archive["snapshots"]
    product.attach(parent, archive, "fork-archive", mode="ro", snapshot_id=snapshot["snapshot_id"])
    source = product.start(parent)
    sources = product.disks(source)
    originals = {"fork-data": private["volume_id"], "fork-persistent": persistent["volume_id"]}
    for key, volume_id in originals.items():
        product.write(source, sources[volume_id][0], f"captured-{key}")
    platform.guest(source, ["/bin/sh", "-ec",
        "printf inherited >/run/a12-fork-marker; printf captured-root >/etc/a12-fork-root; sync"])
    platform.wait_capability(parent, "state.independent_fork")

    fork = product.applied(parent + "/forks", dict(child_name="Independent machine volume", require_independent=True),
                           machine=parent)
    child = "machines/" + fork["result"]["child_machine_id"]
    child_receipt, = fork["children"]
    assert child_receipt["resource_independence"] == "verified", child_receipt
    target = product.execution(child)
    leases = {item["device_key"]: item for item in platform.app(child + "/volume-attachments")["items"]}
    assert set(leases) == set(originals) | {"fork-archive"}
    assert all(lease["state"] == "attached" for lease in leases.values())
    destinations = product.disks(target)
    before, after = {}, {}
    for key, volume_id in originals.items():
        child_volume = leases[key]["volume_id"]
        assert child_volume != volume_id and leases[key]["mode"] == "rw"
        original, cloned = product.read_volume(volume_id), product.read_volume(child_volume)
        before[key], after[key] = original, cloned
        assert original["lifecycle"] == cloned["lifecycle"]
        if key == "fork-data":
            assert original["machine_instance_id"] == source["instance_id"]
            assert cloned["machine_instance_id"] == target["instance_id"]
        assert original["head_resource_id"] != cloned["head_resource_id"]
        target_disk, actual = destinations[child_volume]
        assert actual["writable_head_id"] == cloned["head_resource_id"]
        captured = f"captured-{key}"
        assert product.read(target, target_disk, len(captured)) == captured.encode()
        source_disk = sources[volume_id][0]
        product.write(source, source_disk, "parent-next")
        product.write(target, target_disk, "child-next")
        assert product.read(source, source_disk, 11) == b"parent-next"
        assert product.read(target, target_disk, 10) == b"child-next"
    assert platform.guest(target, ["/bin/cat", "/run/a12-fork-marker"]) == b"inherited"
    root_volume, = set(destinations) - {item["volume_id"] for item in leases.values()}
    root_head = destinations[root_volume][1]["writable_head_id"]
    platform.guest(target, ["/bin/sh", "-ec", "printf child-root >/etc/a12-fork-root; sync"])
    archive_disk, archive_binding = destinations[archive_id]
    assert leases["fork-archive"]["mode"] == "ro" and leases["fork-archive"]["volume_id"] == archive_id
    assert leases["fork-archive"]["generation_id"] == archive_binding["generation_id"] == snapshot["generation_id"]
    assert len(product.read_volume(archive_id)["attachments"]) == 2
    assert platform.guest(target, ["/usr/sbin/blockdev", "--getro", archive_disk]) == b"1\n"
    assert product.read(target, archive_disk, len(archive_mark)) == archive_mark.encode()

    product.stop(parent, source)
    product.applied(parent, machine=parent, method="DELETE")
    source_private = before["fork-data"]
    child_private = after["fork-data"]
    wait_for(lambda: product.head_collected(source_private["head_resource_id"]))
    platform.app("volumes/" + source_private["volume_id"], status=404)
    assert product.read_volume(before["fork-persistent"]["volume_id"])["attachments"] == []
    assert product.read(target, archive_disk, len(archive_mark)) == archive_mark.encode()
    restarted, epochs = target, []
    for _ in range(2):
        product.stop(child, restarted)
        prior = restarted["execution_epoch"]
        restarted = product.start(child)
        assert restarted["execution_epoch"] == prior + 1
        epochs.append(restarted["execution_epoch"])
        actual_disks = product.disks(restarted)
        assert actual_disks[root_volume][1]["writable_head_id"] == root_head
        assert platform.guest(restarted, ["/bin/cat", "/etc/a12-fork-root"]) == b"child-root"
        for cloned in after.values():
            disk, restarted_binding = actual_disks[cloned["volume_id"]]
            assert restarted_binding["writable_head_id"] == cloned["head_resource_id"]
            assert product.read(restarted, disk, 10) == b"child-next"
        assert product.read_volume(child_private["volume_id"])["machine_instance_id"] == restarted["instance_id"]
        assert product.read(restarted, actual_disks[archive_id][0], len(archive_mark)) == archive_mark.encode()
    product.stop(child, restarted)
    product.applied(child, machine=child, method="DELETE")
    wait_for(lambda: product.head_collected(child_private["head_resource_id"]))
    platform.app("volumes/" + child_private["volume_id"], status=404)
    for saved in (before["fork-persistent"], after["fork-persistent"]):
        actual = product.read_volume(saved["volume_id"])
        assert actual["state"] == "available" and actual["head_resource_id"] == saved["head_resource_id"]
        assert not actual["attachments"]
    archive = product.read_volume(archive_id)
    assert archive["snapshots"] == [snapshot] and not archive["attachments"]
    print(json.dumps({"case": "machine-fork", "capture_id": fork["result"]["capture_id"],
                      "parent_volumes": {key: item["volume_id"] for key, item in before.items()},
                      "child_volumes": {key: item["volume_id"] for key, item in after.items()},
                      "parent_collected": source_private["head_resource_id"],
                      "child_collected": child_private["head_resource_id"], "child_restarted_epochs": epochs,
                      "child_root_head": root_head,
                      "shared_immutable": snapshot["generation_id"]},
                     sort_keys=True), flush=True)


def native_roles_case(platform):
    product = Product(platform)

    def request_digest(key):
        return "sha256:" + hashlib.sha256(key.encode()).hexdigest()

    def native_operation(key, kind, execution, parameters):
        expected = {field: execution[field] for field in ("instance_id", "incarnation_id", "execution_epoch")}
        accepted = platform.native("/operations", dict(operation_id=key, kind=kind, expected=expected,
                                                         request_digest=request_digest(key), parameters=parameters))
        assert accepted["phase"] == "NEW", accepted
        receipt = wait_for(lambda: row if (row := platform.native(f"/operations/{key}"))["phase"] != "NEW" else None,
                           timeout=150)
        assert receipt["phase"] == "COMPLETED", receipt
        return receipt

    base = "fixture-all-role-base"
    path = platform.root / "role-data.raw"
    marker = "immutable-data-device"
    with path.open("wb") as image:
        image.truncate(MIB)
        image.seek(4096)
        image.write(marker.encode())
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id, operation_id="import-role-data",
                 request_digest=digest(path), expected=dict(instance_id="role-template", incarnation_id=None, execution_epoch=0))
    worker(platform.worker, "BeginOperation", {"kind": "catalog"}, **scope)
    with path.open("rb") as image:
        worker(platform.worker, "ImportImmutableBlock",
               dict(resource_id=base, capacity_bytes=MIB, content_sha256=scope["request_digest"]), fd=image.fileno(), **scope)
    worker(platform.worker, "FinishOperation", {"abort": False, "expected_revision": 1}, **scope)

    name = "all-block-roles"
    roles = ("root", "storage", "overlay", "cache", "data")
    expected = platform.native(f"/machines/{name}/binding")
    disks = [dict(attachment_id=f"fixture-{role}", device_id=role, role=role, read_only=role == "data",
                  generation_id="fixture-system-root" if role == "root" else base) for role in roles]
    ref = native_operation("launch-all-roles", "launch", expected, dict(
        name=name, boot_profile="system-debian-v1", cpus=1, memory_mib=512, disks=disks, init="/sbin/init"))["execution"]
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))

    def inventory_by_role(execution):
        disks = product.disks(execution)
        by_role = {attachment["role"]: (device, attachment) for device, attachment in disks.values()}
        assert set(by_role) == set(roles) and len(disks) == len(roles), by_role
        return by_role

    observed = inventory_by_role(ref)
    original_ids = {}
    for role, (device, attachment) in observed.items():
        assert attachment["device_id"] == role and attachment["attachment_id"] == f"fixture-{role}"
        assert platform.guest(ref, ["/usr/sbin/blockdev", "--getro", device]) == (b"1\n" if role == "data" else b"0\n")
        original_ids[role] = attachment["writable_head_id"] or attachment["generation_id"]
        if role not in ("root", "data"):
            product.write(ref, device, f"original-{role}")
    platform.guest(ref, ["/bin/sh", "-ec", "printf original-root >/etc/a12-volume-root; sync; printf captured-ram >/run/a12-volume-ram"])
    assert product.read(ref, observed["data"][0], len(marker)) == marker.encode()

    saved = native_operation("snapshot-all-roles", "vm_snapshot", ref, {})["statepoint"]
    assert saved["phase"] == "durable", saved
    point = worker(platform.worker, "QueryStatePoint", {"capture_id": saved["capture_id"]},
                   route_id="deep", bundle_digest=platform.bundle_id, operation_id="snapshot-all-roles",
                   request_digest=request_digest("snapshot-all-roles"), expected=ref)["point"]
    captured = {item["block"]["managed_attachment"]["role"]: item["block"]["managed_attachment"]
                for item in point["devices"] if item.get("block")}
    assert set(captured) == set(roles)
    assert all(original_ids[role] in point["disk_generations"] for role in roles), point["disk_generations"]

    # Move each writable role forward on the live source before restoring.
    platform.guest(ref, ["/bin/sh", "-ec", "printf later-root >/etc/a12-volume-root; sync; printf latest-ram >/run/a12-volume-ram"])
    for role in ("storage", "overlay", "cache"):
        product.write(ref, observed[role][0], f"later-{role}")
    current = inventory_by_role(ref)
    assert platform.guest(ref, ["/bin/cat", "/etc/a12-volume-root"]) == b"later-root"
    for role in ("storage", "overlay", "cache"):
        latest = f"later-{role}"
        assert product.read(ref, current[role][0], len(latest)) == latest.encode()
    stop = native_operation("stop-all-roles", "stop", ref, {})
    assert stop["native"]["source_disposition"] == "EXITED", stop
    name = "restored-block-roles"
    restored = native_operation("restore-all-roles", "vm_restore",
                                platform.native(f"/machines/{name}/binding"),
                                dict(name=name, capture_id=saved["capture_id"], mode="clone_conflicts"))
    assert restored["statepoint"]["manifest_digest"] == saved["manifest_digest"]
    target = restored["execution"]
    devices = inventory_by_role(target)
    assert platform.guest(target, ["/bin/cat", "/etc/a12-volume-root"]) == b"original-root"
    assert platform.guest(target, ["/bin/cat", "/run/a12-volume-ram"]) == b"captured-ram"
    for role, (device, attachment) in devices.items():
        readonly = role == "data"
        assert platform.guest(target, ["/usr/sbin/blockdev", "--getro", device]) == (b"1\n" if readonly else b"0\n")
        if readonly:
            assert attachment["generation_id"] == current[role][1]["generation_id"] == base
            assert product.read(target, device, len(marker)) == marker.encode()
        else:
            assert attachment["volume_id"] != current[role][1]["volume_id"]
            assert attachment["writable_head_id"] != current[role][1]["writable_head_id"]
            if role != "root":
                expected_marker = f"original-{role}"
                assert product.read(target, device, len(expected_marker)) == expected_marker.encode()
    print(json.dumps({"capture_id": saved["capture_id"], "manifest_digest": saved["manifest_digest"],
                      "captured_roles": original_ids, "target_roles": {role: item for role, (_, item) in devices.items()}},
                     sort_keys=True), flush=True)


class FaultPlatform(Platform):
    """Load the precise fsync fault only in this fixture's Worker process."""

    objects = None

    def spawn(self, command, log):
        if Path(command[0]).name != "smolvm-resourced":
            return super().spawn(command, log)
        if self.objects is None:
            # Platform.start binds the catalog before it spawns the Worker.
            # Mount the object filesystem afterwards so that bind cannot hide it.
            self.objects = isolate_objects(self.root, self.stack)
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
    objects.mkdir(mode=0o700)
    subprocess.run(["mount", "-o", "loop", image, objects], check=True)
    stack.callback(lambda: wait_for(lambda: subprocess.run(
        ["umount", objects], capture_output=True).returncode == 0, timeout=10))
    objects.chmod(0o700)
    assert objects.stat().st_dev != objects.parent.stat().st_dev, "object filesystem shares the catalog device"
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
    object_space, catalog_space = (os.statvfs(path) for path in (objects, objects.parent))
    object_available = object_space.f_bavail * object_space.f_frsize
    catalog_available = catalog_space.f_bavail * catalog_space.f_frsize
    assert objects.stat().st_dev != objects.parent.stat().st_dev and catalog_available > 0
    assert platform.broker.poll() is None and worker(platform.worker, "Health")["state"] == "ready"
    print(f"Object ext4 has {object_available} available bytes. The separate catalog filesystem has "
          f"{catalog_available} available bytes and Worker is ready.", flush=True)
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
    pending_volume = product.read_volume(ident)
    assert pending_volume["state"] == "changing" and pending_volume["capacity_bytes"] == 16 * MIB
    assert pending_volume["head_resource_id"] == head and pending_volume["revision"] == original["revision"]
    runtime_name = platform.app(machine)["runtime_name"]
    assert platform.native(f"/machines/{runtime_name}/binding")["incarnation_id"] is None
    rival = product.failure("CONFLICT", f"volumes/{ident}/snapshots", revision=original["revision"])
    assert not row("SELECT operation_id FROM product_volume_operations WHERE operation_id=?", rival["operation_id"])
    assert row("SELECT count(*) FROM product_volume_snapshots") == (0,)
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
    assert settled["result"]["block_capacity_applied"] and settled["result"]["filesystem_resize_status"] == "not_requested"
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
    parser.add_argument("--case", choices=("checkpoint", "detached-checkpoint", "deleted-checkpoint", "lifecycle",
                                           "machine-fork", "native-roles", "growth-fault"),
                        default="checkpoint")
    parser.add_argument("--fsync-shim", type=lambda path: Path(path).resolve(strict=True),
                        help="prebuilt shared library from tests/fixtures/volume-fsync-fault.c")
    parser.add_argument("--verify-retired-head", action="store_true",
                        help="check physical private-head deletion in the deleted-checkpoint case")
    parser.add_argument("--machine-volumes", action="store_true",
                        help="assert machine-lifetime volumes and typed stop requirements on newer runtimes")
    parser.add_argument("--work-dir", type=lambda path: Path(path).resolve(strict=True), default=Path("/var/tmp"))
    parser.add_argument("--use-installation", action="store_true", help="read-only bind the supplied Bundle")
    parser.add_argument("--state-tmpfs", action="store_true", help="use a private tmpfs for platform registry state")
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root, KVM, and systemd")
    if os.environ.get("MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE") != "1":
        os.execvp("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--", "/usr/bin/env",
                              "MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE=1", "PYTHONDONTWRITEBYTECODE=1",
                              sys.executable, __file__, *sys.argv[1:]])
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="volume-proof-") as tmp, ExitStack() as stack:
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
            platform = FaultPlatform(args, root, stack, state_tmpfs=args.state_tmpfs, use_installation=args.use_installation)
        else:
            platform = Platform(args, root, stack, state_tmpfs=args.state_tmpfs, use_installation=args.use_installation)
        try:
            platform.start()
            manifest = json.loads((args.bundle / "runtime-bundle.json").read_text())
            expected = next(entry["sha256"] for entry in manifest["artifacts"] if entry["path"] == "smolvm-resourced")
            assert platform.bundle_id == manifest["bundle_digest"]
            assert worker(platform.worker, "Health")["executable_sha256"] == "sha256:" + expected
            assert Path(f"/proc/{platform.broker.pid}/exe").resolve(strict=True) == platform.bundle / "smolvm-resourced"
            print(f"Live Bundle {platform.bundle_id}, Worker sha256:{expected}, "
                  f"platformd {digest(args.platformd)}, System image {digest(args.system_image)}.", flush=True)
            if args.case.endswith("checkpoint"):
                source_disposition = {"checkpoint": "running", "detached-checkpoint": "stopped", "deleted-checkpoint": "deleted"}[args.case]
                checkpoint_case(platform, check_catalog=not args.checkpoint_data_only, source_disposition=source_disposition,
                                verify_retired_head=args.verify_retired_head)
            elif args.case == "lifecycle":
                lifecycle_case(platform)
            elif args.case == "machine-fork":
                machine_fork_case(platform)
            elif args.case == "native-roles":
                native_roles_case(platform)
            else:
                growth_fault_case(platform, platform.objects)
        except BaseException:
            for log in [root / "platform.log", root / "worker.log", root / "serve.log",
                        *(root / "state/managed").glob("*.log")]:
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-4500:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
