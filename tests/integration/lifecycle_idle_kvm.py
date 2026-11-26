#!/usr/bin/env python3
"""Verify public idle policies on the same Linux/KVM System machine."""

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
import time

from managed_system_vsock import Platform, wait_for, worker


def protection_cut(platform, path, original, apply, protection, side):
    apply("start-automatic-hibernate", {"auto_hibernate": True, "hibernate_after_seconds": 2})
    with closing(sqlite3.connect((platform.root / "catalog/catalog.sqlite").as_uri() + "?mode=ro", uri=True)) as catalog:
        def cut():
            row = catalog.execute("""SELECT b.operation_id,b.original,b.stage,p.phase,b.capture_id FROM managed_fork_batches b
                JOIN lifecycle_decisions d ON d.id=b.operation_id LEFT JOIN statepoints p USING(capture_id)
                WHERE b.source_incarnation=? AND d.kind='hibernate' AND d.origin='idle'""",
                (original["incarnation_id"],)).fetchone()
            if row and (side == "before" or row[3] in ("stop_committed", "hibernated")):
                return row

        operation, source_power, stage, phase, capture = wait_for(cut, timeout=65)
        assert source_power == "RUNNING" and ((phase in ("stop_committed", "hibernated")) == (side == "after"))
        print("Automatic Hibernate boundary", json.dumps(dict(operation=operation, stage=stage, point=phase)), flush=True)
        lease = None
        if protection == "keep-running":
            apply("protect-candidate", {"keep_running": True})
            projected = platform.app(path + "/lifecycle")
            assert projected["applied"]["keep_running"] and projected["desired_revision"] == projected["applied_revision"]
        else:
            view = platform.app(path)
            lease = platform.app(path + "/leases", "POST", dict(reason="Owner cut lease", semantics_version=2, ttl_seconds=5),
                                 {"If-Match": f'"{view["revision"]}"', "Idempotency-Key": "acquire-at-cut",
                                  "If-Control-Epoch": platform.epoch}, timeout=80)
            assert lease["state"] == "active"
            renewed = platform.app("leases/" + lease["lease_id"], "PATCH", {
                "lease_token": lease["lease_token"], "ttl_seconds": 60,
            }, {"If-Match": f'"{lease["revision"]}"', "Idempotency-Key": "renew-at-cut", "If-Control-Epoch": platform.epoch})
            assert renewed["revision"] > lease["revision"] and renewed["state"] == "active"

        def terminal():
            receipt = platform.native("/operations/" + operation)
            return receipt if receipt["phase"] in ("COMPLETED", "ABORTED") else None

        settled = wait_for(terminal)
        assert settled["phase"] == ("ABORTED" if side == "before" else "COMPLETED"), settled
        if protection == "keep-running" and side == "after":
            saved = wait_for(lambda: view if (view := platform.app(path))["power_state"] == "hibernated" else None)
            started = platform.complete(platform.change(path + "/start", "owner-start-after-commit", {}, saved))
            assert started["result"]["statepoint"]["capture_id"] == capture
        machine = path.split("/")[-1]
        _, current = platform.context(machine)
        assert current["instance_id"] == original["instance_id"]
        assert current["execution_epoch"] == original["execution_epoch"] + int(side == "after")
        assert (current["incarnation_id"] == original["incarnation_id"]) == (side == "before")
        if lease:
            assert lease["execution_epoch"] == renewed["execution_epoch"] == current["execution_epoch"]

        def state():
            return worker(platform.worker, "GetLifecycleStatus", route_id="deep", bundle_digest=platform.bundle_id, expected=current)

        # Cross the original five-second TTL and the two-second idle deadline.
        end = time.monotonic() + 8
        blocker = "keep_running" if protection == "keep-running" else "lease:Owner cut lease"
        while time.monotonic() < end:
            observed = state()
            assert observed["power"] == "RUNNING" and blocker in observed["blockers"], observed
            time.sleep(.35)
        assert platform.guest(current, ["/bin/cat", "/run/a28-marker"]) == b"lifecycle-memory"
        all_sleeps = catalog.execute("""SELECT id FROM lifecycle_decisions WHERE instance_id=? AND kind='hibernate'
                                      AND origin='idle'""", (current["instance_id"],)).fetchall()
        assert all_sleeps == [(operation,)], all_sleeps
        if lease:
            released = platform.app("leases/" + lease["lease_id"], "DELETE", {"lease_token": lease["lease_token"]},
                                    {"If-Match": f'"{renewed["revision"]}"', "Idempotency-Key": "release-cut-lease",
                                     "If-Control-Epoch": platform.epoch})
            assert released["state"] == "released"
        print(json.dumps(dict(bundle=platform.bundle_id, protection=protection, side=side, capture=capture,
                              decision=settled["phase"], original_epoch=original["execution_epoch"],
                              active_epoch=current["execution_epoch"], blocker=blocker,
                              owner_renewed=lease is not None, automatic_candidates=len(all_sleeps))), flush=True)


def test(args, root, stack):
    network = dict(network_id="idle-net", route_domain_id="idle-route", cidr="10.64.39.0/24")
    platform = Platform(args, root, stack, (network,), state_tmpfs=True, use_installation=True)
    platform.start()
    initial = dict(auto_standby=False, auto_hibernate=False, min_running_after_wake_seconds=0,
                   standby_after_seconds=2, hibernate_after_seconds=6, pressure_reclaim="disabled")
    created = platform.change("machines", "a28-create", dict(name="Idle policy fixture", template_id="t_system",
        boot_mode="system", runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=1, memory_mib=512),
        network=dict(network_id="idle-net", mode="native"), lifecycle_policy=initial))
    platform.complete(created)
    machine = created["machine_id"]
    path = "machines/" + machine
    _, ref = platform.context(machine)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n", retry=(OSError, RuntimeError))
    with closing(sqlite3.connect(root / "platform/platform.db")) as db:
        binding, generation, namespace = db.execute("SELECT b.id,b.generation,b.task_namespace FROM machines m "
            "JOIN runtime_bindings b ON b.id=m.current_binding_id WHERE m.id=?", (machine,)).fetchone()
    scope = dict(machine_id=machine, generation=generation, task_namespace=namespace, nonce=binding)
    bound = json.loads(platform.guest(ref, ["/usr/local/bin/taskctl", "rpc"], json.dumps(dict(method="bind", binding=scope))))
    assert bound["binding"] == scope, bound
    platform.guest(ref, ["/bin/sh", "-ec", "printf lifecycle-memory > /run/a28-marker"])

    def read(method, payload=None):
        return worker(platform.worker, method, payload, route_id="deep", bundle_digest=platform.bundle_id, expected=ref)

    def status():
        return read("GetLifecycleStatus")

    def during(power, *, timeout=40, ready=lambda state: True):
        def check():
            observed = status()
            return observed if observed["power"] == power and ready(observed) else None
        try:
            return wait_for(check, timeout=timeout, retry=(OSError, RuntimeError))
        except TimeoutError as error:
            raise AssertionError(status()) from error

    def apply(key, patch, *, wait=True):
        view = platform.app(path)
        op = platform.change(path, key, {
            "lifecycle_policy": patch, "expected_policy_revision": view["lifecycle_policy"]["desired_revision"],
        }, view, method="PATCH")
        if wait:
            platform.complete(op)
        return op

    if args.protection:
        return protection_cut(platform, path, ref, apply, args.protection, args.side)

    def wake(key, advances):
        nonlocal ref
        before = ref
        expected = "hibernated" if advances else "standby"
        wait_for(lambda: platform.app(path)["power_state"] == expected, timeout=30)
        action = "start" if advances else "wake"
        platform.complete(platform.change(path + "/" + action, key, {}, platform.app(path)))
        _, ref = platform.context(machine)
        assert ref["instance_id"] == before["instance_id"] and ref["execution_epoch"] == before["execution_epoch"] + advances
        assert (ref["incarnation_id"] == before["incarnation_id"]) == (advances == 0), (before, ref)
        assert platform.guest(ref, ["/bin/cat", "/run/a28-marker"]) == b"lifecycle-memory"
        if advances:
            fresh = during("RUNNING", ready=lambda state: len(state["collectors"]) == 3
                           and all(entry["fresh"] for entry in state["collectors"].values()))
            assert fresh["incarnation_id"] == ref["incarnation_id"], fresh

    def last_event():
        return max((ev["seq"] for ev in read("QueryLifecycleEvents", {"after": 0})["events"]), default=0)

    def finished_after(cursor):
        return [ev["kind"] for ev in read("QueryLifecycleEvents", {"after": cursor})["events"]
                if ev["origin"] == "idle" and ev["stage"] == "completed"]

    def saved_after(cursor, expected):
        state = during("HIBERNATED", timeout=80, ready=lambda row: row["statepoint"] and finished_after(cursor) == expected)
        assert state["incarnation_id"] is None, state
        return state

    during("RUNNING", ready=lambda state: not state["blockers"])
    cursor = last_event()
    previous = platform.app(path)["lifecycle_policy"]["applied_revision"]
    platform.broker.send_signal(signal.SIGSTOP)
    try:
        operation = apply("standby-only", {"auto_standby": True}, wait=False)
        pending = wait_for(lambda: row if (row := platform.app(path)["lifecycle_policy"])["desired_revision"] > previous else None)
        assert pending["effective_status"] == "pending" and pending["applied_revision"] == previous, pending
        assert platform.app("operations/" + operation["operation_id"])["state"] != "succeeded"
    finally:
        platform.broker.send_signal(signal.SIGCONT)
    platform.complete(operation)
    asleep = during("STANDBY", ready=lambda state: state["controlled_suspended"])
    assert asleep["effective_policy"]["auto_standby"] and not asleep["effective_policy"]["auto_hibernate"], asleep
    time.sleep(7)
    held = status()
    assert held["power"] == "STANDBY" and held["statepoint"] is None and held["next_action"] is None, held
    assert finished_after(cursor) == ["standby"], finished_after(cursor)
    print("Unacknowledged policy was pending. Standby alone kept the same parked execution past the Hibernate timeout without creating a StatePoint.", flush=True)
    apply("disable-after-standby", {"auto_standby": False})
    wake("wake-standby-only", 0)

    cursor = last_event()
    apply("hibernate-only", {"auto_hibernate": True})
    saved = saved_after(cursor, ["hibernate"])
    assert saved["statepoint"] and saved["incarnation_id"] is None and not saved["effective_policy"]["auto_standby"], saved
    assert finished_after(cursor) == ["hibernate"], finished_after(cursor)
    print("Hibernate alone saved and fenced the running execution without scheduling Standby.", flush=True)
    apply("disable-after-hibernate", {"auto_hibernate": False})
    wake("wake-hibernate-only", 1)

    cursor = last_event()
    apply("both-stages", {"auto_standby": True, "auto_hibernate": True, "standby_after_seconds": 4, "hibernate_after_seconds": 30})
    running = during("RUNNING", ready=lambda state: state["idle_ms"] is not None and state["next_action"] and state["next_action"]["kind"] == "standby")
    origin = time.monotonic() * 1000 - running["idle_ms"]
    asleep = during("STANDBY", ready=lambda state: state["controlled_suspended"] and state["next_action"]
                    and state["next_action"]["kind"] == "hibernate")
    assert abs(time.monotonic() * 1000 - asleep["idle_ms"] - origin) < 1000, (running, asleep)
    assert asleep["next_action"]["kind"] == "hibernate" and abs(asleep["idle_ms"] + asleep["next_action"]["eligible_after_ms"] - 30000) < 250, asleep
    ttl_expired = during("STANDBY", ready=lambda state: state["controlled_suspended"] and not state["collectors"]["guest"]["fresh"])
    assert not ttl_expired["blockers"], ttl_expired
    saved = saved_after(cursor, ["standby", "hibernate"])
    assert saved["statepoint"] and finished_after(cursor) == ["standby", "hibernate"], (saved, finished_after(cursor))
    print("Both timers used one observed monotonic idle origin. Managed Standby remained controlled after the Guest report expired and the Worker completed Hibernate.", flush=True)
    apply("disable-after-two-stages", {"auto_standby": False, "auto_hibernate": False})
    wake("wake-two-stages", 1)

    apply("hold-evidence", {"auto_standby": True, "auto_hibernate": True, "standby_after_seconds": 2, "hibernate_after_seconds": 12})
    asleep = during("STANDBY", ready=lambda state: state["controlled_suspended"]
                    and state["last_decision"]["stage"] == "completed"
                    and state["next_action"] and state["next_action"]["kind"] == "hibernate")
    assert asleep["next_action"]["eligible_after_ms"] > 0, asleep
    cursor = last_event()
    with closing(sqlite3.connect(root / "catalog/catalog.sqlite")) as db:
        row = db.execute("SELECT control_socket FROM native_endpoints WHERE incarnation_id=?", (ref["incarnation_id"],)).fetchone()
    endpoint = Path(row[0])
    if endpoint.is_relative_to("/run/smolvm-custom"):
        assert endpoint.samefile(root / "runtime" / endpoint.relative_to("/run/smolvm-custom")), endpoint
    else:
        assert endpoint.is_relative_to(root), endpoint
    assert endpoint.is_socket(), endpoint
    withheld = endpoint.with_name(endpoint.name + ".offline")
    endpoint.rename(withheld)
    try:
        unknown = during("UNKNOWN", ready=lambda state: "unknown:native_inventory" in state["blockers"])
        assert not unknown["controlled_suspended"] and unknown["next_action"] is None, unknown
        projected = platform.app(path + "/lifecycle")
        assert projected["effective_status"] == "blocked" and "unknown:native_inventory" in projected["effective_blockers"], projected
        wait_for(lambda: not status()["collectors"]["guest"]["fresh"], timeout=25)
        assert finished_after(cursor) == [], finished_after(cursor)
    finally:
        withheld.rename(endpoint)
    stale = during("STANDBY", ready=lambda state: "unknown:guest" in state["blockers"])
    assert not stale["controlled_suspended"] and stale["idle_ms"] is None and stale["next_action"] is None, stale
    assert stale["incarnation_id"] == ref["incarnation_id"] and finished_after(cursor) == [], stale
    print("Loss of the registered native socket invalidated the parked hold before the Hibernate deadline. Restoring the socket alone did not revive the expired Guest evidence.", flush=True)
    apply("disable-after-lost-hold", {"auto_standby": False, "auto_hibernate": False})
    wake("wake-lost-hold", 0)

    cursor = last_event()
    raw = "a28-raw-profile"
    platform.native("/operations", dict(operation_id=raw,
        request_digest="sha256:" + hashlib.sha256(raw.encode()).hexdigest(),
        expected=ref, kind="standby", parameters={"mode": "raw"}))
    raw_receipt = wait_for(lambda: receipt if (receipt := platform.native("/operations/" + raw))["phase"] != "NEW" else None)
    assert raw_receipt["phase"] == "COMPLETED", raw_receipt
    apply("hibernate-under-raw-profile", {"auto_hibernate": True, "hibernate_after_seconds": 3})
    raw_state = during("STANDBY", ready=lambda state: "unknown:standby_hold" in state["blockers"])
    reported = platform.app(path + "/lifecycle")
    assert not raw_state["controlled_suspended"] and raw_state["next_action"] is None, raw_state
    assert reported["effective_status"] == "blocked" and "unknown:standby_hold" in reported["effective_blockers"], reported
    time.sleep(4)
    assert finished_after(cursor) == [], finished_after(cursor)
    print("The real raw profile had no managed hold proof. The platform explained the blocked policy and the Worker made no automatic transition.", flush=True)
    apply("disable-after-raw", {"auto_hibernate": False, "pressure_reclaim": "idle_only"})
    wake("wake-raw-profile", 0)

    cursor = last_event()
    legacy = {"enabled": True, "timeout_seconds": 5}
    platform.complete(platform.change(path, "legacy-idle-request", {"idle_policy": legacy}, platform.app(path), method="PATCH"))
    policy = platform.app(path + "/lifecycle")
    assert policy["source"] == "legacy" and not policy["applied"]["auto_standby"], policy
    assert policy["applied"]["auto_hibernate"] and policy["applied"]["hibernate_after_seconds"] == 5, policy
    assert policy["applied"]["pressure_reclaim"] == "disabled" and platform.app(path)["idle_policy"] == legacy, policy
    saved = saved_after(cursor, ["hibernate"])
    assert saved["statepoint"] and finished_after(cursor) == ["hibernate"], (saved, finished_after(cursor))
    print("The historical idle_policy request used Hibernate alone and disabled automatic pressure reclaim. All cases used one logical machine on", platform.bundle_id, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(option, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--protection", choices=("keep-running", "lease"))
    parser.add_argument("--side", choices=("before", "after"))
    args = parser.parse_args()
    if bool(args.protection) != bool(args.side):
        parser.error("--protection and --side select a protection race together")
    if os.geteuid() != 0:
        parser.error("requires root, KVM and a live systemd manager")
    if os.environ.get("A28_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]], {**os.environ, "A28_KVM_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="iid-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        catalog = root / "catalog"
        catalog.mkdir()
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G,mode=0700,nosuid,nodev", "tmpfs", catalog], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", catalog], check=True)
        if args.protection:
            unit = f"smolvmcutpolicy{os.getpid()}.slice"
            subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                            "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                            unit, "fail", "1", "MemoryAccounting", "b", "true", "0"], stdout=subprocess.DEVNULL, check=True)
            stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
            os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        try:
            test(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3800:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
