#!/usr/bin/env python3
"""Verify public managed Volume operations and exact StatePoint data on KVM."""

import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile

from managed_system_vsock import Platform, digest, wait_for, worker


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


def checkpoint_case(platform, *, check_catalog=True):
    product = Product(platform)
    machine = product.machine("Checkpoint disks")
    ids = []
    for name in ("left", "right"):
        volume = product.volume(name)
        ids.append(volume["volume_id"])
        product.attach(machine, volume, name)
    archive = product.volume("read-only archive")
    archive_id = archive["volume_id"]
    archive = product.applied(f"volumes/{archive_id}/snapshots", revision=archive["revision"])["result"]["volume"]
    archived = archive["snapshots"][0]
    archive_lease = product.attach(machine, archive, "archive", mode="ro", snapshot_id=archived["snapshot_id"])
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

    exact = product.failure("VOLUME_GENERATION_MISMATCH", "restores",
                            {"name": "Exact conflict", "snapshot_id": snapshot, "volume_restore_policy": "exact"})
    print(f"Exact public restore returned {exact['error']['code']} and effect none.", flush=True)
    assert platform.context(machine.split("/")[-1])[1] == ref
    cloned = product.applied("restores", {"name": "Saved disk clone", "snapshot_id": snapshot,
                                         "volume_restore_policy": "clone_conflicts"})
    clone = "machines/" + cloned["machine_id"]
    restored = product.execution(clone)
    assert restored["instance_id"] != ref["instance_id"] and restored["execution_epoch"] == 1
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
        assert product.read(ref, advanced[ident][0], len(current)) == current.encode()
        product.write(restored, target, "clone-only")
        assert product.read(ref, advanced[ident][0], len(current)) == current.encode()
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
        assert archive_lease["attachment_id"] in {item["attachment_id"] for item in shared}
        assert {item["instance_id"] for item in shared} == {ref["instance_id"], restored["instance_id"]}
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

    inventory = platform.native(f"/instances/{first['instance_id']}/inventory")
    pidfd = os.pidfd_open(inventory["execution"]["pid"])
    try:
        product.applied(machine + "/stop", machine=machine)
        assert select.select([pidfd], [], [], 4)[0], inventory["execution"]
    finally:
        os.close(pidfd)
    assert platform.app(machine)["power_state"] == "stopped"
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--checkpoint-data-only", action="store_true",
                        help="omit projection assertions when measuring a historical installation")
    parser.add_argument("--case", choices=("checkpoint", "lifecycle"), default="checkpoint")
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
        platform = Platform(args, root, stack)
        try:
            platform.start()
            manifest = json.loads((args.bundle / "runtime-bundle.json").read_text())
            expected = next(entry["sha256"] for entry in manifest["artifacts"] if entry["path"] == "smolvm-resourced")
            assert platform.bundle_id == manifest["bundle_digest"]
            assert worker(platform.worker, "Health")["executable_sha256"] == "sha256:" + expected
            print(f"Live Bundle {platform.bundle_id}, Worker sha256:{expected}, "
                  f"platformd {digest(args.platformd)}.", flush=True)
            if args.case == "checkpoint":
                checkpoint_case(platform, check_catalog=not args.checkpoint_data_only)
            else:
                lifecycle_case(platform)
        except BaseException:
            for log in [root / "platform.log", root / "worker.log", root / "serve.log",
                        *(root / "state/managed").glob("*.log")]:
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-4500:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
