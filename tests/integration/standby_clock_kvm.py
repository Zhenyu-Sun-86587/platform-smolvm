#!/usr/bin/env python3
"""Verify exact Standby, capture and Wake clocks on one installed Linux/KVM bundle."""

import argparse
from contextlib import ExitStack, closing
import hashlib
import json
import os
from pathlib import Path
import select
import sqlite3
import subprocess
import sys
import tempfile
import time

from lifecycle_wake_collector_kvm import readonly_bind
from managed_system_vsock import Platform, digest, wait_for


LONG_STANDBY = 24
TIMER_SECONDS = 9


def stage_development_bundle(args, root, stack):
    # This development overlay keeps the historical installation byte-identical.
    measured = root / "measured"
    measured.mkdir()
    readonly_bind(stack, args.bundle, measured)
    manifest = json.loads((measured / "runtime-bundle.json").read_bytes())
    artifacts = {entry["path"]: entry for entry in manifest["artifacts"]}

    def replace(path, source, checksum):
        artifacts[path].update(size=source.stat().st_size, sha256=checksum.removeprefix("sha256:"))
        readonly_bind(stack, source, measured / path)

    def replace_profile(name, update):
        path = f"profiles/{name}.json"
        profile = json.loads((measured / path).read_bytes())
        profile.update(update)
        source = root / f"{name}.json"
        source.write_text(json.dumps(profile))
        checksum = digest(source)
        replace(path, source, checksum)
        return checksum

    if args.library:
        checksum = digest(args.library)
        for path in ("lib/libkrun.so", "lib/libkrun.so.2", "lib/libkrun.so.2.0.0"):
            replace(path, args.library, checksum)
        contracts = manifest["contracts"]
        contracts["state_reader_id"] = "standby-clock-development:" + checksum
        manifest["verification_profile_digest"] = replace_profile("verification", {
            "state_reader_id": contracts["state_reader_id"],
        })
        contracts["clock_contract_digest"] = replace_profile("clock", {
            "schema": "RuntimeClockContract/2", "standby_checkpoint": "SMVCSBY1",
            "standby_clock": "freeze-through-activation", "verified_operations": [],
        })
        print("Testing development libkrun overlay", checksum, flush=True)
    if args.worker:
        checksum = digest(args.worker)
        replace("smolvm-resourced", args.worker, checksum)
        print("Testing development Worker overlay", checksum, flush=True)
    manifest.pop("bundle_digest")
    manifest.pop("signature", None)
    manifest["bundle_digest"] = "sha256:" + hashlib.sha256(json.dumps(
        manifest, separators=(",", ":"), sort_keys=True).encode()).hexdigest()
    updated = root / "runtime-bundle.json"
    updated.write_text(json.dumps(manifest))
    readonly_bind(stack, updated, measured / "runtime-bundle.json")
    args.bundle = measured


def test(args, root, stack):
    platform = Platform(args, root, stack, state_tmpfs=True, use_installation=True)
    platform.start()
    start = platform.change("machines", "a08-create", {
        "name": "Standby clock fixture", "template_id": "t_system", "boot_mode": "system",
        "runtime_instance_id": platform.serve_id, "start": True, "resources": {"cpu_count": 1, "memory_mib": 384},
        "lifecycle_policy": {"auto_standby": False, "auto_hibernate": False, "keep_running": True},
    })
    platform.complete(start)
    original = start["machine_id"]
    path = "machines/" + original
    _, ref = platform.context(original)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
    probe = args.probe
    if probe is None:
        probe = root / "clock-probe"
        sample_source = Path(__file__).resolve().parents[1] / "fixtures/standby-clock.c"
        subprocess.run([args.cc, "-static", "-Os", "-s", "-o", probe, sample_source], check=True)
    clock = platform.place_probe(ref, probe)
    catalog = root / "catalog"
    clock_failures = []

    def clock_check(ok, label, detail):
        if not ok:
            report = f"{label}: {detail}"
            clock_failures.append(report)
            print(report, flush=True)

    def native(kind, scope, key, parameters=None):
        return platform.native("/operations", {
            "kind": kind, "expected": scope, "operation_id": key, "parameters": parameters or {},
            "request_digest": "sha256:" + hashlib.sha256(key.encode()).hexdigest(),
        })

    def finished(key):
        result = wait_for(lambda: row if (row := platform.native("/operations/" + key))["phase"] != "NEW" else None)
        assert result["phase"] == "COMPLETED", result
        return result

    def inventory(scope):
        return platform.native(f"/instances/{scope['instance_id']}/inventory")

    def hardware(scope, disposition):
        result = inventory(scope)
        observed = result["execution"]
        assert {k: observed[k] for k in scope} == scope, observed
        assert result["inventory"]["source_disposition"] == disposition, result
        return observed["pid"], observed["pid_start_time"]

    def held(scope):
        with closing(sqlite3.connect(catalog / "catalog.sqlite")) as db:
            return db.execute("""SELECT n.operation_id,n.mode,n.session,n.stage FROM native_standby s
                JOIN native_transitions n USING(operation_id) WHERE n.incarnation_id=?""", (scope["incarnation_id"],)).fetchone()

    def resources():
        with closing(sqlite3.connect(catalog / "catalog.sqlite")) as db:
            metadata = tuple(db.execute(sql).fetchall() for sql in (
                "SELECT resource_id,kind,state,durability,immutable,descriptor_digest FROM resources ORDER BY resource_id",
                "SELECT volume_id,head_resource_id,backing_identity FROM volume_heads ORDER BY volume_id",
                "SELECT capture_id,representation_id,phase,manifest_digest FROM statepoints ORDER BY capture_id",
            ))
        objects = tuple(sorted(str(p.relative_to(catalog)) for p in (catalog / "objects").rglob("*") if p.is_file()))
        return metadata, objects

    def wait_platform(power):
        return wait_for(lambda: row if (row := platform.app(path))["power_state"] == power else None, timeout=30)

    def numbers(raw):
        values = tuple(int(n) for n in raw.split())
        assert len(values) == 3, raw
        return values

    def sample(scope):
        return numbers(platform.guest(scope, [clock]))

    def output(scope, file):
        return platform.guest(scope, ["/bin/cat", file])

    def begin_timer(scope, label):
        marker = "/run/a08-" + label
        launched_at = time.monotonic()
        platform.guest(scope, ["/usr/bin/systemd-run", "--unit=a08-" + label, "/bin/sh", "-ec",
            'read -r pid rest < /proc/self/stat; printf "%s\\n" "$pid" > "$2.pid"; '
            '"$1" > "$2.start"; /bin/sleep "$3"; "$1" > "$2.end"',
            "timer", clock, marker, str(TIMER_SECONDS)])
        start = wait_for(lambda: output(scope, marker + ".start"), timeout=5, retry=(RuntimeError, OSError))
        pid = output(scope, marker + ".pid").strip()
        assert pid.isdigit() and int(pid) > 1, pid
        return marker, numbers(start), pid, launched_at

    def resume_sample(scope, timer, label):
        marker, beginning, pid, _ = timer
        assert output(scope, marker + ".pid").strip() == pid
        assert numbers(output(scope, marker + ".start")) == beginning
        alive = platform.guest(scope, ["/bin/sh", "-c", 'if test -e "$1" && ! test -e "$2"; then printf pending; fi',
            "test", "/proc/" + pid.decode().strip(), marker + ".end"])
        observed = sample(scope)
        deltas = tuple((after - before) / 1e9 for after, before in zip(observed, beginning))
        clock_check(alive == b"pending" and all(0 <= elapsed < TIMER_SECONDS for elapsed in deltas),
                    label, f"timer had already ended or Guest clocks skipped ahead at first sample {deltas}")
        # Debian's system service leaves real time on the guest's KVM clock.
        clock_check(max(deltas) - min(deltas) < 1, label, f"Guest clocks diverged at first sample {deltas}")
        print(f"{label}: Guest clock elapsed {deltas[0]:.3f}s monotonic, {deltas[1]:.3f}s boottime and {deltas[2]:.3f}s realtime after the held interval.", flush=True)
        return observed, time.monotonic()

    def confirm_clock(scope, timer, observation, label):
        previous, began = observation
        time.sleep(1.2)
        now = sample(scope)
        active = time.monotonic() - began
        deltas = tuple((after - before) / 1e9 for after, before in zip(now, previous))
        clock_check(all(0.8 < elapsed and abs(elapsed - active) < 0.75 for elapsed in deltas), label,
                    f"clock slopes {deltas}s Guest / {active:.3f}s host active time")
        fired = numbers(wait_for(lambda: output(scope, timer[0] + ".end"), timeout=18, retry=(RuntimeError, OSError)))
        elapsed = (fired[0] - timer[1][0]) / 1e9
        clock_check(TIMER_SECONDS <= elapsed < TIMER_SECONDS + 2, label, f"timer fired after {elapsed:.3f}s Guest time")
        print(f"{label}: Guest clock advanced {deltas[0]:.3f}s in {active:.3f}s host active time and its original timer fired at {elapsed:.3f}s Guest time.", flush=True)

    def pause_managed(label):
        timer = begin_timer(ref, label)
        ids = resources()
        process = hardware(ref, "RUNNING")
        platform.complete(platform.change(path + "/standby", label + "-standby", {}, platform.app(path)))
        assert time.monotonic() - timer[3] < 5, "the timer was armed too early to isolate the held interval"
        parked = held(ref)
        assert parked and parked[1] == "managed" and parked[3] == "complete", parked
        assert hardware(ref, "STANDBY") == process
        assert resources() == ids
        time.sleep(LONG_STANDBY)
        assert held(ref) == parked and hardware(ref, "STANDBY") == process
        assert resources() == ids, "Standby changed the RAM, durable objects or writer generations"
        return timer, parked, process

    raw_timer = begin_timer(ref, "raw")
    initial = resources()
    assert initial[0][0] and initial[0][1] and initial[1], initial
    original_pid = hardware(ref, "RUNNING")
    native("standby", ref, "raw-standby", {"mode": "raw"})
    raw_receipt = finished("raw-standby")
    assert time.monotonic() - raw_timer[3] < 5, "the raw timer was armed too early to isolate the held interval"
    assert raw_receipt["native"]["mode"] == "raw", raw_receipt
    raw_hold = held(ref)
    assert raw_hold and raw_hold[1] == "raw", raw_hold
    assert hardware(ref, "STANDBY") == original_pid and resources() == initial
    time.sleep(12)
    try:
        native("independent_fork", ref, "raw-forbidden-fork", {"children": ["raw-forbidden"]})
    except RuntimeError as error:
        _, status, body = error.args[0]
        failure = json.loads(body)
        assert status == 409 and failure["code"] == "E_STATE_CONFLICT" and "managed standby" in failure["error"], failure
    else:
        raise AssertionError("raw Standby accepted a strict independent fork")
    assert held(ref) == raw_hold and hardware(ref, "STANDBY") == original_pid and resources() == initial
    native("wake", ref, "raw-wake")
    raw_wake = finished("raw-wake")
    assert native("wake", ref, "raw-wake") == raw_wake
    assert hardware(ref, "RUNNING") == original_pid
    confirm_clock(ref, raw_timer, resume_sample(ref, raw_timer, "Raw Wake"), "Raw Wake")
    wait_platform("running")
    print("Raw Standby rejected strict fork while preserving its original hold, graph and VMM. Identical Wake replay returned its original receipt.", flush=True)

    timer, parked, process = pause_managed("fork")
    fork = platform.complete(platform.change(path + "/forks", "managed-parked-fork",
        {"child_name": "Held clock child", "require_independent": True}, platform.app(path)))
    assert fork["source_disposition"] == "STANDBY" and fork["source_lock_released"], fork
    assert held(ref) == parked and hardware(ref, "STANDBY") == process
    child_id = fork["children"][0]["machine_id"]
    child_path = "machines/" + child_id
    _, child = platform.context(child_id)
    assert child["incarnation_id"] != ref["incarnation_id"] and child["instance_id"] != ref["instance_id"], fork
    child_clock = resume_sample(child, timer, "Fork child")
    headers = {"If-Control-Epoch": platform.epoch, "If-Match": f'"{platform.app(path)["revision"]}"', "Idempotency-Key": "managed-held-wake"}
    body = {"expected_generation": platform.app(path)["binding_generation"]}
    wake_request = platform.app(path + "/wake", "POST", body, headers, 202)
    managed_wake = platform.complete(wake_request)
    assert platform.app(path + "/wake", "POST", body, headers, 202)["operation_id"] == wake_request["operation_id"]
    assert managed_wake["state"] == "succeeded" and hardware(ref, "RUNNING") == process
    parent_clock = resume_sample(ref, timer, "Managed Wake")
    confirm_clock(child, timer, child_clock, "Fork child")
    confirm_clock(ref, timer, parent_clock, "Managed Wake")
    wait_platform("running")
    platform.complete(platform.change(child_path + "/stop", "retire-clock-child", {}, platform.app(child_path)))
    print("Managed Fork preserved the original hold. The public Wake replay kept the same execution.", flush=True)

    timer, parked, process = pause_managed("hibernate")
    with os.fdopen(os.pidfd_open(process[0]), "rb", buffering=0) as source:
        accepted = platform.change(path + "/hibernate", "a08-hibernate", {}, platform.app(path))
        slept = platform.complete(accepted)
        proof = platform.native("/operations/" + accepted["operation_id"])["statepoint"]
        assert proof["original_source_disposition"] == "STANDBY" and proof["phase"] == "hibernated", proof
        assert proof["manifest_digest"] and slept["source_disposition"] == "EXITED", slept
        assert select.select([source], [], [], 5)[0], "Hibernate left the source VMM live"
    wait_platform("hibernated")
    # Leave the saved point offline for longer than the pending guest timer.
    time.sleep(TIMER_SECONDS + 3)
    assert platform.native("/operations/" + accepted["operation_id"])["statepoint"]["manifest_digest"] == proof["manifest_digest"]
    platform.complete(platform.change(path + "/start", "a08-restore", {}, platform.app(path)))
    _, restored = platform.context(original)
    assert restored["instance_id"] == ref["instance_id"] and restored["incarnation_id"] != ref["incarnation_id"], restored
    assert restored["execution_epoch"] == ref["execution_epoch"] + 1, restored
    confirm_clock(restored, timer, resume_sample(restored, timer, "Hibernate restore"), "Hibernate restore")
    print("Managed Hibernate used the held point, fenced the source and restored the original timer markers at the next epoch.", flush=True)
    assert not clock_failures, clock_failures
    print("A08 verified bundle", platform.bundle_id, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(key, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--cc", default="cc")
    parser.add_argument("--probe", type=lambda path: Path(path).resolve(), help="prebuilt static standby-clock.c")
    parser.add_argument("--library", type=lambda path: Path(path).resolve(), help="development libkrun overlay")
    parser.add_argument("--worker", type=lambda path: Path(path).resolve(), help="development Worker overlay")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM and a live systemd manager")
    if os.environ.get("STANDBY_CLOCK_KVM_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]], {**os.environ, "STANDBY_CLOCK_KVM_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="stk-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        catalog = root / "catalog"
        catalog.mkdir()
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=3500M,mode=0700,nosuid,nodev", "tmpfs", catalog], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", catalog], check=True)
        try:
            if args.library or args.worker:
                stage_development_bundle(args, root, stack)
            test(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3500:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
