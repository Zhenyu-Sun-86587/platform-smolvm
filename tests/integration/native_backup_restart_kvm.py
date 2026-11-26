#!/usr/bin/env python3
"""Restart a public native backup across both pin boundaries on Debian KVM."""

import argparse
from contextlib import ExitStack, closing, contextmanager
import grp
import hashlib
import json
import os
from pathlib import Path
import select
import signal
import sqlite3
import subprocess
import sys
import tempfile

from backup_publication_crash import BackupFiles
from managed_system_vsock import Platform, UnixHTTPConnection, EXT, digest, request, wait_for, worker
from volume_product_kvm import Product
from worker_restart_kvm import cpu_operation as scoped_operation, request_fault, vmm_pid


class BackupPlatform(Platform):
    def spawn(self, command, log):
        if log == "platform.log" and not hasattr(self, "backup_key"):
            self.config = Path(command[-1])
            self.backup_key = self.root / "recovery.key"
            with open(self.backup_key, "xb", opener=lambda p, flags: os.open(p, flags, 0o600)) as file:
                file.write(os.urandom(32))
            cfg = json.loads(self.config.read_text())
            cfg["backups"] = [dict(id="native", scope="durable_runtime", directory=str(self.root / "archives"),
                                   interval_seconds=86400, retain=2, recovery_key_file=str(self.backup_key))]
            if self.args.previous_platformd:
                secondary = self.root / "secondary-api/serve.sock"
                super().spawn([self.bundle / "smolvm-bin", "--state-dir", self.root / "secondary",
                               "serve", "start", "--listen", f"unix://{secondary}", "--worker-resource-socket",
                               self.worker, "--guest-rollout-port", "37452", "--shutdown-grace", "0"], "secondary.log")
                peer = wait_for(lambda: request(UnixHTTPConnection(secondary), "GET", EXT + "/capabilities")[0],
                                retry=(OSError, RuntimeError))
                self.secondary_id = peer["runtime_instance_id"]
                assert self.secondary_id != self.serve_id and peer["bundle_digest"] == self.bundle_id
                cfg["runtime"]["custom"].append({**cfg["runtime"]["custom"][0],
                    "instance_id": self.secondary_id, "socket": str(secondary), "private_networks": []})
                command = [self.args.previous_platformd, *command[1:]]
            self.config.write_text(json.dumps(cfg))
        process = super().spawn(command, log)
        if log == "worker.log":
            self.source_worker = process
        return process

    def start_worker(self, endpoint, state, *, original=True):
        return self.spawn([self.bundle / "smolvm-resourced", "serve", "--socket", endpoint,
                           "--state-dir", state, "--socket-gid", str(grp.getgrnam("kvm").gr_gid)],
                          "worker.log" if original else "recovery-worker.log")

    def submit_backup(self, key):
        return self.app("backups/policies/native/runs", "POST", {}, {
            "Idempotency-Key": key, "If-Control-Epoch": self.epoch,
        }, status=202)["backup_id"]

    def backup(self, ident, *, state="verified", timeout=170):
        def terminal():
            run = self.app("backups/" + ident)
            if run["state"] == "running":
                return None
            assert run["state"] == state, run
            return run
        return wait_for(terminal, timeout=timeout)

    def backup_admin(self, *args):
        result = subprocess.run([self.args.sandboxctl, "admin", "--config", self.config, "backup", *args],
                                env=self.env, text=True, capture_output=True, timeout=190, check=False)
        assert result.returncode == 0, (result.stdout, result.stderr)
        return json.loads(result.stdout)


def kill(process):
    descriptor = os.pidfd_open(process.pid)
    try:
        signal.pidfd_send_signal(descriptor, signal.SIGKILL)
        assert process.wait(timeout=10) == -signal.SIGKILL
    finally:
        os.close(descriptor)


def db_rows(db, sql, *parameters):
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as connection:
        return connection.execute(sql, parameters).fetchall()


def guest_address(platform, execution):
    snapshot = json.loads(platform.guest(execution, ["/usr/sbin/ip", "-j", "-4", "addr", "show", "dev", "eth0"]))
    address, = [a for interface in snapshot for a in interface["addr_info"] if a["scope"] == "global"]
    return address["local"], address["prefixlen"]


@contextmanager
def unavailable_reader(platform):
    # All processes in this fixture share a private mount namespace. Concealing
    # the installation here keeps every file and inode in the release intact.
    with tempfile.TemporaryDirectory(dir=platform.root, prefix="offline-reader-") as empty:
        subprocess.run(["mount", "--bind", empty, platform.bundle], check=True)
        try:
            yield
        finally:
            subprocess.run(["umount", platform.bundle], check=True)


def recovery(platform, archive, native, saved, sleeping, marker, volume, volume_marker, old_execution, origin_address):
    destination = platform.root / "archives" / "independent"
    platform.backup_admin("recover-native", "--archive", archive, "--output", destination, "--worker-catalog")
    catalog = destination / Path(native["directory"]).name
    db = catalog / "catalog.sqlite"
    assert not db_rows(db, "SELECT 1 FROM executions WHERE phase!='exited'")
    for table in ("instance_bindings", "network_tap_leases", "private_packet_wake_rules"):
        assert db_rows(db, f"SELECT count(*) FROM {table}") == [(0,)], table
    for point in (saved, sleeping):
        assert db_rows(db, "SELECT manifest_digest FROM statepoints WHERE capture_id=?", point["capture_id"]) == [
            (point["manifest_digest"],)
        ]
    with (catalog / "objects" / volume / "data").open("rb") as source:
        source.seek(8 * 512)
        assert source.read(len(volume_marker)) == volume_marker
    ipc = platform.root / "recovery-ipc"
    ipc.mkdir(mode=0o755)
    worker_socket, serve_socket = ipc / "worker.sock", platform.root / "recovery-api/serve.sock"
    platform.start_worker(worker_socket, catalog, original=False)
    health = wait_for(lambda: worker(worker_socket, "Health"), retry=(OSError, RuntimeError))
    if health["state"] == "recovering":
        worker(worker_socket, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]})
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id)
    worker(worker_socket, "RegisterPrivateNetwork", dict(network_id="backup-destination",
           route_domain_id="independent-destination", cidr="10.194.0.0/24"), **scope)
    platform.spawn([platform.bundle / "smolvm-bin", "--state-dir", platform.root / "recovery-serve",
                    "serve", "start", "--listen", f"unix://{serve_socket}", "--worker-resource-socket",
                    worker_socket, "--guest-rollout-port", "37346", "--shutdown-grace", "0"], "recovery-serve.log")
    original_socket = platform.serve
    platform.serve = serve_socket
    try:
        wait_for(lambda: platform.native("/capabilities"), retry=(OSError, RuntimeError))
        name = "from-native-archive"
        binding = platform.native(f"/machines/{name}/binding")
        execution = {key: binding[key] for key in ("instance_id", "incarnation_id", "execution_epoch")}
        operation = "a23-independent-restore"
        platform.native("/operations", {"operation_id": operation,
            "request_digest": "sha256:" + hashlib.sha256(operation.encode()).hexdigest(), "expected": execution,
            "kind": "vm_restore", "parameters": {"name": name, "capture_id": saved["capture_id"],
            "mode": "clone_conflicts", "network_id": "backup-destination"}})

        def restored():
            receipt = platform.native("/operations/" + operation)
            assert receipt["phase"] != "ABORTED", receipt
            return receipt if receipt["phase"] == "COMPLETED" else None

        receipt = wait_for(restored, timeout=145)
        ref = platform.native(f"/machines/{name}/execution")
        assert ref["execution_epoch"] == 1 and ref["incarnation_id"] != old_execution["incarnation_id"]
        assert receipt["statepoint"]["manifest_digest"] == saved["manifest_digest"]
        network = receipt["statepoint"]["network_recovery"]
        assert network["mode"] == platform.args.network_mode and network["source"]["route_domain_id"] == "backup-origin", network
        assert network["target"]["route_domain_id"] == "independent-destination", network
        assert network["target"]["attachment_id"] != network["source"]["attachment_id"], network
        assert network["source"]["guest_ipv4"] == origin_address[0], network
        assert network["target"]["private_ipv4"] != network["source"]["private_ipv4"], network
        for file in ("/run/a23-ram", "/root/a23-disk"):
            assert platform.guest(ref, ["/bin/cat", file]).strip() == marker
        observed = guest_address(platform, ref)
        assert observed[0] == network["target"]["guest_ipv4"], (observed, network)
        if network["mode"] == "fixed_internal":
            assert observed == origin_address, (observed, origin_address)
        else:
            assert observed == (network["target"]["private_ipv4"], 32), (observed, network)
        # Reap only the test's exact recovered VMM before its store is removed.
        pid = vmm_pid(platform, ref)
        descriptor = os.pidfd_open(pid)
        try:
            signal.pidfd_send_signal(descriptor, signal.SIGKILL)
            assert select.select([descriptor], [], [], 10)[0]
        finally:
            os.close(descriptor)
        print("Independent", network["mode"], "execution restored both Guest markers and independent volume bytes",
              marker.decode(), "epoch", ref["execution_epoch"], "route", network["target"]["route_domain_id"],
              "Guest IPv4", observed, "private IPv4", network["target"]["private_ipv4"], flush=True)
    finally:
        platform.serve = original_socket


def run(platform):
    root = platform.root
    archives = root / "archives"
    database = root / "platform/platform.db"
    worker_state = root / "catalog"
    catalog = worker_state / "catalog.sqlite"
    platform.start()
    print("Bundle", platform.bundle_id, "Worker", digest(platform.bundle / "smolvm-resourced"),
          "platformd", digest(platform.args.platformd), flush=True)
    wait_for(lambda: any(row["state"] == "verified" for row in platform.app("backups")["items"]))
    created = platform.change("machines", "a23-system", {"name": "Backup handoff", "template_id": "t_system",
        "boot_mode": "system", "runtime_instance_id": platform.serve_id, "start": False,
        "resources": {"cpu_count": 1, "memory_mib": 512},
        "network": {"network_id": "backup-source", "mode": platform.args.network_mode}})
    platform.complete(created)
    machine = "machines/" + created["machine_id"]
    product = Product(platform)
    volume = product.volume("a23 independent volume", 8 << 20)
    attached = product.attach(machine, volume, "export-data")
    ref = product.start(machine)
    device = product.disks(ref)[volume["volume_id"]][0]
    volume_marker = ("a23-volume-" + os.urandom(12).hex()).encode()
    product.write(ref, device, volume_marker.decode())
    assert product.read(ref, device, len(volume_marker)) == volume_marker
    product.stop(machine, ref)
    product.applied("volume-attachments/" + attached["attachment_id"],
                    {"expected_volume_revision": product.read_volume(volume["volume_id"])["revision"]},
                    machine=machine, method="DELETE")
    current = product.read_volume(volume["volume_id"])
    snap = product.applied("volumes/" + volume["volume_id"] + "/snapshots", revision=current["revision"])["result"]["volume"]["snapshots"][0]
    generation = snap["generation_id"]
    assert product.read_volume(volume["volume_id"])["snapshots"][0]["snapshot_id"] == snap["snapshot_id"]
    previous = platform.submit_backup("a23-volume-only")
    platform.backup(previous)
    previous_archive = archives / previous
    previous_manifest = digest(previous_archive / "backup-manifest.json")
    ref = product.start(machine)
    origin_address = guest_address(platform, ref)
    marker = os.urandom(18).hex().encode()
    platform.guest(ref, ["/bin/sh", "-ec", f'echo {marker.decode()} >/run/a23-ram; echo {marker.decode()} >/root/a23-disk; sync'])
    capture = platform.complete(platform.change(machine + "/snapshots", "a23-checkpoint", {"capture_mode": "vm"}, platform.app(machine)))["result"]
    saved = capture["statepoint"]
    sleeping = platform.complete(platform.change(machine + "/hibernate", "a23-hibernate", {}, platform.app(machine)))["result"]["statepoint"]
    assert platform.app(machine)["power_state"] == "hibernated"
    point_block, = [row[0] for row in db_rows(catalog, "SELECT child_resource_id FROM resource_edges WHERE owner_resource_id=?", saved["representation_id"])]
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id)

    # Capture the persistent pending handoff while no Worker can accept a pin.
    first_session = worker(platform.worker, "Health")["broker_session_id"]
    kill(platform.source_worker)
    gate = BackupFiles(archives)
    try:
        ident = platform.submit_backup("a23-selected-closure")
        stage = archives / (".sandbox-" + ident)

        def before_copy(path, _):
            # At this open all Worker routes have acknowledged and no native
            # data file for the selected closure has been copied yet.
            if path.name != "closure.json" or not path.is_relative_to(stage):
                return False
            selected, = db_rows(database, "SELECT catalog_sha256,worker_pinned FROM native_backup_selections WHERE backup_id=?", ident)
            assert selected == (seal, 1), selected
            pins = db_rows(catalog, "SELECT backup_id,selection_json,manifest_json FROM native_backup_exports WHERE backup_id LIKE ? AND phase='pinned'", ident + "-%")
            backup_id, selection_json, manifest_json = pins[0]
            assert len(pins) == 1
            selected_roots = json.loads(selection_json)
            assert set(selected_roots["capture_ids"]) == {saved["capture_id"], sleeping["capture_id"]}
            assert selected_roots["generation_ids"] == [generation]
            attested.append((backup_id, selection_json, manifest_json))
            snapshot = "snapshots/" + capture["snapshot_id"]
            revision = platform.app(snapshot)["revision"]
            deleted = platform.app(snapshot, "DELETE", {}, {"If-Control-Epoch": platform.epoch,
                "If-Match": f'"{revision}"', "Idempotency-Key": "a23-delete-during-staging"}, status=202)
            platform.complete(deleted)
            product.applied("volume-snapshots/" + snap["snapshot_id"], method="DELETE",
                            revision=product.read_volume(volume["volume_id"])["revision"])
            for resource in (point_block, generation, saved["representation_id"], sleeping["representation_id"]):
                info = worker(platform.worker, "QueryResource", {"resource_id": resource}, **scope)
                request_fault("E_RESOURCE_PINNED", lambda: worker(platform.worker, "MarkResourceForGC",
                    {"resource_id": resource, "expected_revision": info["revision"]}, **scope))
            print("Staging protected two captures and the independent volume while public DELETE released their source roots", flush=True)
            return True

        # The fanotify consumer has to admit the staged SQLite copy's earlier
        # opens as well. After the catalog is sealed, this callback can start
        # the same Worker while the platform is still blocked before copying.
        attested = []
        platform.start_worker(platform.worker, worker_state)
        health = wait_for(lambda: worker(platform.worker, "Health"), retry=(OSError, RuntimeError))
        assert health["state"] == "recovering" and health["broker_session_id"] != first_session

        sealed = []
        later_saved = []
        def allow_catalog(path, _):
            record = db_rows(database, "SELECT catalog_sha256,worker_pinned FROM native_backup_selections WHERE backup_id=?", ident)
            if record and record[0][1] == 0:
                assert platform.app("backups/" + ident)["state"] == "running"
                assert db_rows(database, "SELECT snapshot_id FROM native_backup_volume_pins WHERE backup_id=?", ident) == [(snap["snapshot_id"],)]
                assert not db_rows(catalog, "SELECT 1 FROM native_backup_exports WHERE backup_id LIKE ?", ident + "-%")
                sealed.append(record[0][0])
                worker(platform.worker, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]})

                def release_current():
                    return worker(platform.worker, "ReleaseStatePoint", {
                        "capture_id": sleeping["capture_id"], "manifest_digest": sleeping["manifest_digest"],
                    }, **scope)

                request_fault("E_RESOURCE_PINNED", release_current)
                try:
                    rejected = product.operation(machine, machine=machine, method="DELETE")
                    assert rejected["state"] == "failed" and rejected["effect"] == "none", rejected
                except RuntimeError as exc:
                    assert exc.args[0][1] == 409, exc
                assert platform.app(machine)["power_state"] == "hibernated"
                platform.complete(platform.change(machine + "/start", "a23-wake-before-pins", {}, platform.app(machine)))
                request_fault("E_RESOURCE_PINNED", release_current)
                replacement = platform.complete(platform.change(machine + "/hibernate", "a23-hibernate-before-pins", {},
                                                                platform.app(machine)))["result"]["statepoint"]
                assert replacement["capture_id"] != sleeping["capture_id"]
                later_saved.append(replacement)
                original_root = worker(platform.worker, "QueryOwnerClosure", {"owner_kind": "statepoint_hibernate",
                                       "owner_id": sleeping["capture_id"]}, **scope)
                assert any(item["resource_id"] == sleeping["representation_id"] and item["state"] == "available"
                           for item in original_root["resources"]), original_root
                info = worker(platform.worker, "QueryResource", {"resource_id": sleeping["representation_id"]}, **scope)
                request_fault("E_RESOURCE_PINNED", lambda: worker(platform.worker, "MarkResourceForGC",
                    {"resource_id": sleeping["representation_id"], "expected_revision": info["revision"]}, **scope))
                assert not db_rows(catalog, "SELECT 1 FROM native_backup_exports WHERE backup_id LIKE ?", ident + "-%")
                return True
            return False

        gate.at_open(platform.platform, allow_catalog, seconds=70)
        seal, = sealed
        later_sleeping, = later_saved
        print("Original run sealed selection before Worker pins", ident, "catalog SHA-256", seal, flush=True)
        print("Before Worker pins, dormant Delete and Release were fenced. Wake and another Hibernate retained the original cut",
              sleeping["capture_id"], "new current cut", later_sleeping["capture_id"], flush=True)
        gate.at_open(platform.platform, before_copy, kill=True, seconds=110)
    finally:
        gate.close()
    assert digest(stage / "platform.db") == "sha256:" + seal
    backup_id, selection_json, manifest_json = attested[0]

    # Leave an unrelated catalog operation so the Worker cannot enter ready.
    operation = scoped_operation(platform, dict(instance_id="a23-recovery-hold", incarnation_id=None, execution_epoch=0),
                                  "a23-recovery-hold", {})
    worker(platform.worker, "BeginOperation", {"kind": "catalog"}, **operation)
    prior = worker(platform.worker, "Health")["broker_session_id"]
    kill(platform.source_worker)
    offset = len((root / "platform.log").read_text())
    platform.restart()
    wait_for(lambda: "native backup is awaiting Worker reconciliation" in (root / "platform.log").read_text()[offset:], timeout=20)
    assert platform.app("backups/" + ident)["state"] == "running"
    assert digest(stage / "platform.db") == "sha256:" + seal
    platform.start_worker(platform.worker, worker_state)
    health = wait_for(lambda: worker(platform.worker, "Health"), retry=(OSError, RuntimeError))
    assert health["state"] == "recovering" and health["broker_session_id"] != prior
    request_fault("E_RECOVERING", lambda: worker(platform.worker, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]}))
    assert platform.submit_backup("a23-selected-closure") == ident
    platform.backup(ident)
    assert worker(platform.worker, "Health")["state"] == "recovering"
    assert db_rows(catalog, "SELECT phase,selection_json,manifest_json FROM native_backup_exports WHERE backup_id=?", backup_id) == [
        ("released", selection_json, manifest_json)
    ]
    assert not db_rows(database, "SELECT 1 FROM native_backup_selections WHERE backup_id=?", ident)
    archive = archives / ident
    actual = platform.backup_admin("verify", "--archive", archive)
    native, = actual["native_closures"]
    assert actual["backup_id"] == ident and digest(archive / "platform.db") == "sha256:" + seal
    closure = archive / native["directory"] / "closure.json"
    assert closure.read_text() == manifest_json
    verified_manifest = digest(archive / "backup-manifest.json")
    print("Same run and immutable catalog resumed with Worker still recovering; both pins released only after verification", ident, native["manifest_digest"], flush=True)
    worker(platform.worker, "FinishOperation", {"abort": False, "expected_revision": 1}, **operation)
    worker(platform.worker, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]})

    # The current hibernated source is the remaining selection of these runs.
    sleeping_block, = [row[0] for row in db_rows(catalog, "SELECT child_resource_id FROM resource_edges WHERE owner_resource_id=?", later_sleeping["representation_id"])]
    for key in ("missing-block", "missing-reader"):
        with ExitStack() as unavailable:
            if key == "missing-block":
                filename = worker_state / "objects" / sleeping_block / "data"
                hidden = filename.with_name(filename.name + ".fixture-held")
                gate = BackupFiles(archives)
                try:
                    failed = platform.submit_backup("a23-" + key)
                    failed_stage = archives / (".sandbox-" + failed)

                    def remove_member(path, _):
                        if path.name != "closure.json" or not path.is_relative_to(failed_stage):
                            return False
                        assert db_rows(database, "SELECT worker_pinned FROM native_backup_selections WHERE backup_id=?", failed) == [(1,)]
                        filename.rename(hidden)
                        unavailable.callback(hidden.rename, filename)
                        return True

                    gate.at_open(platform.platform, remove_member, seconds=90)
                finally:
                    gate.close()
            else:
                unavailable.enter_context(unavailable_reader(platform))
                failed = platform.submit_backup("a23-" + key)
            assert platform.backup(failed, state="failed")["failure_code"] == "NATIVE_CLOSURE_UNAVAILABLE"
            assert not (archives / failed).exists()
            assert digest(archive / "backup-manifest.json") == verified_manifest
            assert digest(previous_archive / "backup-manifest.json") == previous_manifest
    assert platform.backup_admin("verify", "--archive", archive)["backup_id"] == ident
    print("Missing immutable block and original reader failed new runs while both verified archives remained intact", flush=True)

    platform.complete(platform.change(machine + "/start", "a23-wake-before-retire", {}, platform.app(machine)))
    _, original = platform.context(created["machine_id"])
    product.stop(machine, original)
    product.applied(machine, machine=machine, method="DELETE")
    kill(platform.source_worker)
    recovery(platform, archive, native, saved, sleeping, marker, generation, volume_marker, original, origin_address)


def resume_legacy_routes(platform):
    root = platform.root
    catalog = root / "catalog/catalog.sqlite"
    live = root / "platform/platform.db"
    archives = root / "archives"
    platform.start()
    assert db_rows(live, "PRAGMA user_version") == [(1017,)]
    wait_for(lambda: any(row["state"] == "verified" for row in platform.app("backups")["items"]))
    product = Product(platform)
    independent = {}
    for route in (platform.serve_id, platform.secondary_id):
        accepted = platform.change("volumes", "a23-volume-" + route, {"name": "Separate route volume",
            "capacity_bytes": 1 << 20, "runtime_instance_id": route})
        volume = platform.complete(accepted)["result"]["volume"]
        receipt = product.applied("volumes/" + volume["volume_id"] + "/snapshots", revision=volume["revision"])
        independent[route] = receipt["result"]["volume"]["snapshots"][0]["generation_id"]

    recorded = []
    gate = BackupFiles(archives)
    try:
        ident = platform.submit_backup("a23-legacy-two-routes")
        stage = archives / (".sandbox-" + ident)

        def first_route(path, _):
            if path.name != "closure.json" or not path.is_relative_to(stage):
                return False
            pin, = db_rows(catalog, "SELECT backup_id,selection_json,manifest_json FROM native_backup_exports WHERE backup_id LIKE ? AND phase='pinned'", ident + "-%")
            recorded.append(pin)
            return True

        gate.at_open(platform.platform, first_route, kill=True, seconds=80)
    finally:
        gate.close()
    old_sha = digest(stage / "platform.db")
    assert db_rows(stage / "platform.db", "PRAGMA user_version") == [(1017,)]
    print("Platform schema 1017 interrupted after the first of two Worker routes committed its pin",
          ident, recorded[0][0], old_sha, flush=True)
    platform.restart()
    (upgraded,), = db_rows(live, "PRAGMA user_version")
    assert upgraded >= 1018
    assert platform.submit_backup("a23-legacy-two-routes") == ident
    platform.backup(ident)
    archive = archives / ident
    assert digest(archive / "platform.db") == old_sha
    result = platform.backup_admin("verify", "--archive", archive)
    assert result["backup_id"] == ident and result["schema_version"] == 1017
    assert len(result["native_closures"]) == 2
    for native in result["native_closures"]:
        raw = (archive / native["directory"] / "closure.json").read_text()
        manifest = json.loads(raw)
        assert manifest["generations"] == [independent[native["runtime_instance_id"]]]
        if manifest["backup_id"] == recorded[0][0]:
            assert raw == recorded[0][2]
    rows = db_rows(catalog, "SELECT phase FROM native_backup_exports WHERE backup_id LIKE ?", ident + "-%")
    assert rows == [("released",), ("released",)]
    print("Schema", upgraded, "finished the original run with both real Serve routes and the same schema 1017 archive catalog", flush=True)
    dispatched_volume_delete(platform, set(independent.values()))


def dispatched_volume_delete(platform, retained):
    product = Product(platform)
    volume = product.volume("a23 in-flight deletion", 1 << 20)
    saved = product.applied("volumes/" + volume["volume_id"] + "/snapshots", revision=volume["revision"])["result"]["volume"]
    snapshot, = saved["snapshots"]
    database = platform.root / "platform/platform.db"
    catalog = platform.root / "catalog/catalog.sqlite"
    log = platform.root / "platform.log"
    offset = len(log.read_text())
    descriptor = os.pidfd_open(platform.source_worker.pid)
    try:
        signal.pidfd_send_signal(descriptor, signal.SIGSTOP)
        deleting = platform.app("volume-snapshots/" + snapshot["snapshot_id"], "DELETE", {}, {
            "If-Control-Epoch": platform.epoch, "If-Match": f'"{saved["revision"]}"',
            "Idempotency-Key": "a23-volume-drop-before-pin",
        }, status=202)
        wait_for(lambda: db_rows(database, "SELECT 1 FROM operations WHERE id=? AND json_extract(dispatch_json,'$.volume') IS NOT NULL",
                                  deleting["operation_id"]), timeout=20)
        ident = platform.submit_backup("a23-backup-during-volume-drop")
        stage = platform.root / "archives" / (".sandbox-" + ident)
        wait_for(lambda: "a selected volume snapshot is already being removed" in log.read_text()[offset:], timeout=15)
        assert platform.app("backups/" + ident)["state"] == "running"
        assert not stage.with_name(stage.name + ".catalog-ready").exists()
        assert not db_rows(database, "SELECT 1 FROM native_backup_selections WHERE backup_id=?", ident)
        assert not db_rows(catalog, "SELECT 1 FROM native_backup_exports WHERE backup_id LIKE ?", ident + "-%")
    finally:
        signal.pidfd_send_signal(descriptor, signal.SIGCONT)
        os.close(descriptor)
    platform.complete(deleting)
    assert platform.submit_backup("a23-backup-during-volume-drop") == ident
    platform.backup(ident)
    archive = platform.root / "archives" / ident
    result = platform.backup_admin("verify", "--archive", archive)
    generations = set()
    for native in result["native_closures"]:
        raw = (archive / native["directory"] / "closure.json").read_text()
        generations.update(json.loads(raw)["generations"])
    assert generations == retained and snapshot["generation_id"] not in generations
    print("Dispatched public volume deletion kept backup unsealed; after it committed the same run selected only retained generations",
          ident, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ("bundle", "platformd", "sandboxctl", "system-image"):
        parser.add_argument("--" + key, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--previous-platformd", type=lambda path: Path(path).resolve(),
                        help="run the two-route in-flight migration with a schema 1017 binary")
    parser.add_argument("--network-mode", choices=("native", "fixed_internal"), default="native")
    parser.add_argument("--use-installation", action="store_true", help="expose the installed Bundle read-only")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, fanotify, Linux KVM, and a live systemd manager")
    if os.environ.get("NATIVE_BACKUP_FIXTURE_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "NATIVE_BACKUP_FIXTURE_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a23-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        for name, capacity in (("archives", "6G"), ("catalog", "4G")):
            path = root / name
            path.mkdir(mode=0o700)
            subprocess.run(["mount", "-t", "tmpfs", "-o", f"size={capacity},mode=0700,nosuid,nodev", "tmpfs", path], check=True)
            stack.callback(subprocess.run, ["umount", "--lazy", path], check=True)
        cgroup = f"smolvmbackup{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        cgroup, "fail", "1", "MemoryAccounting", "b", "true", "0"], check=True,
                       stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", cgroup], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = cgroup
        platform = BackupPlatform(args, root, stack, state_tmpfs=True, use_installation=args.use_installation,
                                  networks=(dict(network_id="backup-source", route_domain_id="backup-origin", cidr="10.112.0.0/24"),))
        try:
            if args.previous_platformd:
                resume_legacy_routes(platform)
            else:
                run(platform)
        except BaseException:
            for log in (*root.glob("*.log"), *(root / "state/managed").glob("*.log"), *(root / "recovery-serve/managed").glob("*.log")):
                print(log.name, log.read_text(errors="replace")[-6500:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
