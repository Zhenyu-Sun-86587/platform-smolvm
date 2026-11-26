#!/usr/bin/env python3
"""Verify collector wake fencing and fresh platform lifecycle projections on KVM."""

import argparse
from contextlib import ExitStack, closing
import hashlib
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import sys
import tempfile

from managed_system_vsock import Platform, digest, wait_for, worker


def readonly_bind(stack, source, target):
    subprocess.run(["mount", "--bind", source, target], check=True)
    stack.callback(subprocess.run, ["umount", "--lazy", target], check=True)
    subprocess.run(["mount", "-o", "remount,bind,ro", target], check=True)


def stage_worker(args, root, stack):
    # Replace only the Worker and its manifest entry in this private mount
    # namespace. The frozen installation stays byte-identical.
    b = root / "measured"
    b.mkdir()
    readonly_bind(stack, args.bundle, b)
    original = b / "runtime-bundle.json"
    manifest = json.loads(original.read_bytes())
    artifact = next(item for item in manifest["artifacts"] if item["path"] == "smolvm-resourced")
    artifact.update(size=args.worker.stat().st_size, sha256=digest(args.worker).removeprefix("sha256:"))
    manifest.pop("bundle_digest")
    manifest.pop("signature", None)
    manifest["bundle_digest"] = "sha256:" + hashlib.sha256(json.dumps(
        manifest, separators=(",", ":"), sort_keys=True).encode()).hexdigest()
    updated = root / "runtime-bundle.json"
    updated.write_text(json.dumps(manifest))
    readonly_bind(stack, args.worker, b / "smolvm-resourced")
    readonly_bind(stack, updated, original)
    args.bundle = b


def test(args, root, stack):
    updated_platform = args.platformd
    if args.upgrade_from:
        args.platformd = args.upgrade_from
    network = dict(network_id="wake-net", route_domain_id="wake-route", cidr="10.64.38.0/24")
    platform = Platform(args, root, stack, (network,), state_tmpfs=True, use_installation=True)
    platform.start()
    policy = {"auto_standby": True, "auto_hibernate": False, "standby_after_seconds": 300,
              "min_running_after_wake_seconds": 0}
    if args.upgrade_from:
        policy["keep_running"] = True
    else:
        policy["pressure_reclaim"] = "disabled"
    launch = platform.change("machines", "create-collector-vm", {
        "name": "Wake collector fixture", "template_id": "t_system", "boot_mode": "system",
        "runtime_instance_id": platform.serve_id, "start": True, "resources": {"cpu_count": 1, "memory_mib": 512},
        "network": {"network_id": "wake-net", "mode": "native"},
        "lifecycle_policy": policy,
    })
    platform.complete(launch)
    machine = launch["machine_id"]
    path = "machines/" + machine
    _, ref = platform.context(machine)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n", retry=(OSError, RuntimeError))
    with closing(sqlite3.connect(root / "platform/platform.db")) as db:
        binding, generation, namespace = db.execute("SELECT b.id,b.generation,b.task_namespace FROM machines m "
            "JOIN runtime_bindings b ON b.id=m.current_binding_id WHERE m.id=?", (machine,)).fetchone()
    task_scope = dict(machine_id=machine, generation=generation, task_namespace=namespace, nonce=binding)
    bound = json.loads(platform.guest(ref, ["/usr/local/bin/taskctl", "rpc"], json.dumps(dict(method="bind", binding=task_scope))))
    assert bound["binding"] == task_scope
    platform.guest(ref, ["/bin/sh", "-ec", "printf instance-marker > /run/wake-collector-marker"])

    def worker_call(method, payload=None, key=None):
        request = dict(route_id="deep", bundle_digest=platform.bundle_id, expected=ref)
        if key:
            request.update(operation_id=key, request_digest="sha256:" + hashlib.sha256(key.encode()).hexdigest())
        return worker(platform.worker, method, payload, **request)

    def state():
        return worker_call("GetLifecycleStatus")

    if args.upgrade_from:
        original = wait_for(lambda: row if (row := platform.app(path)["lifecycle_policy"])["applied_revision"] >= 1 else None)
        platform.broker.send_signal(signal.SIGSTOP)
        try:
            op = platform.change(path, "old-unacknowledged-idle-policy",
                {"idle_policy": {"enabled": True, "timeout_seconds": 300}}, platform.app(path), method="PATCH")
            old = wait_for(lambda: row if (row := platform.app(path)["lifecycle_policy"])["source"] == "legacy" else None)
            assert old["desired_revision"] == original["applied_revision"] + 1 and old["applied_revision"] == original["applied_revision"], old
            assert old["desired"]["pressure_reclaim"] == "idle_only" and "pressure_reclaim" not in old["overrides"], old
            platform.platform_command[0] = updated_platform
            platform.restart()
            pending = platform.app(path + "/lifecycle")
            assert pending["desired_revision"] == old["desired_revision"] + 1 and pending["applied_revision"] == 0, pending
            assert pending["effective_status"] == "pending" and pending["desired"]["pressure_reclaim"] == "disabled", pending
        finally:
            platform.broker.send_signal(signal.SIGCONT)
        platform.complete(op)
        migrated = platform.app(path + "/lifecycle")
        applied = state()
        assert migrated["applied_revision"] == migrated["desired_revision"] == applied["applied_revision"], (migrated, applied)
        assert applied["effective_policy"]["pressure_reclaim"] == "disabled" and applied["effective_policy"]["auto_hibernate"], applied
        platform.complete(platform.change(path, "configure-short-wake-fixture", {
            "lifecycle_policy": {"auto_standby": True, "auto_hibernate": False, "keep_running": False},
            "expected_policy_revision": migrated["desired_revision"],
        }, platform.app(path), method="PATCH"))
        print("A real 1017 legacy policy pending at upgrade was reapplied as Hibernate-only with pressure disabled, and its original operation completed.", flush=True)

    def ready():
        s = state()
        return s if s["power"] == "RUNNING" and not s["blockers"] and len(s["collectors"]) == 3 and all(
            entry["fresh"] for entry in s["collectors"].values()) else None

    def reject(key, payload):
        try:
            worker_call("ReportActivity", payload, key)
        except RuntimeError as error:
            assert error.args[0]["code"] == "E_STALE_REVISION", error
            return error.args[0]["message"]
        raise AssertionError(f"accepted stale collector {key}")

    for turn in range(2):
        previous = wait_for(ready, timeout=35, retry=(OSError, RuntimeError))
        old_generation = previous.get("collector_generation")
        old = previous["collectors"]["guest"]
        candidate = dict(collector="guest", session=old["session"], seq=old["seq"] + 100,
            valid_for_ms=20000, counts=dict(task=0, exec=0, inbound=0, outbound=0))
        if old_generation:
            candidate["generation"] = old_generation
        platform.complete(platform.change(path + "/standby", f"short-standby-{turn}", {}, platform.app(path)))
        parked = state()
        assert parked["power"] == "STANDBY" and parked["collectors"]["guest"]["fresh"], parked
        platform.platform.send_signal(signal.SIGSTOP)
        try:
            # Drive the same native operation the public handler submits, with
            # the reporter frozen so no post-Wake sample can race this check.
            key = f"controlled-wake-{turn}"
            platform.native("/operations", dict(kind="wake", expected=ref, operation_id=key,
                request_digest="sha256:" + hashlib.sha256(key.encode()).hexdigest(), parameters={}))
            receipt = wait_for(lambda: result if (result := platform.native("/operations/" + key))["phase"] != "NEW" else None)
            assert receipt["phase"] == "COMPLETED", receipt
            after = state()
            assert after["power"] == "RUNNING" and after["collector_generation"] == key, after
            assert after["incarnation_id"] == ref["incarnation_id"] and after["execution_epoch"] == ref["execution_epoch"], after
            assert not after["collectors"] and after["next_action"] is None, after
            assert {"unknown:guest", "unknown:host", "unknown:platform"} <= set(after["blockers"]), after
            assert "generation changed" in reject(f"late-known-{turn}", candidate)
            assert "generation changed" in reject(f"late-new-{turn}", {
                **candidate, "session": f"previous-generation-unused-{turn}", "seq": 1})
            assert "session has been retired" in reject(f"reuse-old-{turn}", {
                **candidate, "generation": key, "seq": 1})
        finally:
            platform.platform.send_signal(signal.SIGCONT)
        fresh = wait_for(ready, timeout=35, retry=(OSError, RuntimeError))
        assert all(fresh["collectors"][name]["session"] != previous["collectors"][name]["session"]
                   for name in fresh["collectors"]), (fresh, previous)
        assert platform.context(machine)[1] == ref
        assert platform.guest(ref, ["/bin/cat", "/run/wake-collector-marker"]) == b"instance-marker"
    print("Two short Standby wakes kept the exact execution and RAM. All three collectors rotated, and stale-generation or retired-session reports were refused.", flush=True)

    def views():
        return platform.app(path)["lifecycle_policy"], platform.app(path + "/lifecycle")

    healthy = views()
    assert all(row["effective_status"] == "applied" and row["worker"]["collectors"] for row in healthy), healthy
    platform.broker.send_signal(signal.SIGSTOP)
    try:
        missing = views()
        assert all(row["effective_status"] == "blocked" and row["effective_blockers"] == ["unknown:worker"]
                   and "worker" not in row for row in missing), missing
    finally:
        platform.broker.send_signal(signal.SIGCONT)
    again = views()
    assert all(row["effective_status"] == "applied" and row["worker"]["power"] == "RUNNING" for row in again), again
    print("Both public views suppressed stale cached Worker status during the outage and again reported applied after a live observation.", flush=True)
    print("Collector wake fencing verified on measured Worker bundle", platform.bundle_id, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(key, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--worker", type=lambda path: Path(path).resolve(),
                        help="stage a measured development Worker over the installed runtime")
    parser.add_argument("--upgrade-from", type=lambda path: Path(path).resolve(),
                        help="start with a schema 1017 platform binary and migrate its pending legacy policy")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM and a live systemd manager")
    if os.environ.get("WAKE_COLLECTOR_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]], {**os.environ, "WAKE_COLLECTOR_KVM_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="wcf-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        catalog = root / "catalog"
        catalog.mkdir()
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=3G,mode=0700,nosuid,nodev", "tmpfs", catalog], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", catalog], check=True)
        if args.worker:
            stage_worker(args, root, stack)
        try:
            test(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3500:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
