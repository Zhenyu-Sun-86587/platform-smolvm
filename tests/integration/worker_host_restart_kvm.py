#!/usr/bin/env python3
"""Recover a saved System guest and fence live volatile generations on a new Worker boot."""

import argparse
from contextlib import ExitStack
import fcntl
import grp
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time

from managed_system_vsock import Platform, digest, wait_for, worker
from release_host_reboot import boot, collect_modules


SEED = Path("/mnt/release-fixture")
ROOT = Path("/opt/worker-proof")
TIMER_SECONDS = 18
TIMER_PREFIX = "/run/worker-reboot-timer"


def rows(platform, query, values=()):
    path = platform.root / "catalog/catalog.sqlite"
    with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as db:
        return db.execute(query, values).fetchall()


def create_system(platform, name):
    op = platform.change("machines", "create-" + name, dict(name=name, boot_mode="system", template_id="t_system",
        start=True, runtime_instance_id=platform.serve_id, resources=dict(cpu_count=1, memory_mib=512)))
    platform.complete(op)
    path = "machines/" + op["machine_id"]
    _, ref = platform.context(op["machine_id"])
    wait_for(lambda: platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
    return path, ref


def park_timer(platform, machine, execution):
    probe = platform.place_probe(execution, platform.args.clock_probe)
    platform.guest(execution, ["systemd-run", "--unit=worker-reboot-timer", "sh", "-ec",
        'read -r pid rest < /proc/self/stat; echo "$pid" > "$2.pid"; "$1" > "$2.start"; sleep "$3"; "$1" > "$2.end"',
        "clock", probe, TIMER_PREFIX, str(TIMER_SECONDS)])
    before = wait_for(lambda: platform.guest(execution, ["cat", TIMER_PREFIX + ".start"]),
                      retry=(OSError, RuntimeError))
    pid = int(platform.guest(execution, ["cat", TIMER_PREFIX + ".pid"]))
    assert pid > 1
    platform.complete(platform.change(machine + "/standby", "park-before-host-reboot", {}, platform.app(machine)))
    time.sleep(TIMER_SECONDS + 8)
    inventory = platform.native(f"/instances/{execution['instance_id']}/inventory")
    assert inventory["inventory"]["source_disposition"] == "STANDBY", inventory
    return dict(probe=probe, pid=pid, start=list(map(int, before.split())))


def verify_timer(platform, execution, timer):
    script = ('cat "$2.pid" "$2.start"; if test -e "$2.end" || ! test -d "/proc/$3"; then echo ended; '
              'else echo pending; fi; "$1"')
    raw = platform.guest(execution, ["sh", "-ec", script, "clock", timer["probe"], TIMER_PREFIX, str(timer["pid"])])
    pid, before, disposition, now = raw.splitlines()
    assert int(pid) == timer["pid"] and list(map(int, before.split())) == timer["start"], raw
    def elapsed(sample):
        return [(n - start) / 1e9 for n, start in zip(map(int, sample.split()), timer["start"], strict=True)]
    first = elapsed(now)
    assert disposition == b"pending" and all(0 <= t < TIMER_SECONDS for t in first), (raw, first)
    assert max(first) - min(first) < 1, first
    final = wait_for(lambda: platform.guest(execution, ["cat", TIMER_PREFIX + ".end"]),
                     timeout=TIMER_SECONDS + 12, retry=(OSError, RuntimeError))
    fired = elapsed(final)
    assert all(TIMER_SECONDS <= t < TIMER_SECONDS + 3 for t in fired), fired
    print("Saved timer", timer["pid"], "was still pending after Worker boot change; monotonic, boottime and realtime",
          first, "seconds after arming at first sample; fired at", fired, flush=True)


def first_boot(platform):
    password = os.urandom(24).hex()
    secret = platform.root / "reboot-owner-password"
    with open(secret, "x", opener=lambda name, flags: os.open(name, flags, 0o600)) as f:
        f.write(password)
    platform.start(owner_password=password)
    boot_id = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    print("Worker host", boot_id, "launched bundle", platform.bundle_id, flush=True)
    sleepy, saved_ref = create_system(platform, "saved")
    nonce = os.urandom(16).hex()
    platform.guest(saved_ref, ["/bin/sh", "-ec",
        'printf "%s" "$1" > /run/volatile-marker; printf "%s" "$1" > /root/durable-marker; sync', "write", nonce])
    timer = park_timer(platform, sleepy, saved_ref) if platform.args.clock_probe else None
    view = platform.wait_capability(sleepy, "state.hibernate")
    saved = platform.complete(platform.change(sleepy + "/hibernate", "save-before-power-loss", {}, view))
    point = saved["result"]["statepoint"]
    if timer:
        cut = platform.native("/operations/" + saved["operation_id"])["statepoint"]
        assert cut["original_source_disposition"] == "STANDBY" and cut["capture_id"] == point["capture_id"], cut
    print("Saved nested Debian", point["capture_id"], point["manifest_digest"], flush=True)
    source, ref = create_system(platform, "volatile")
    view = platform.wait_capability(source, "state.fork_batch")
    batch = platform.complete(platform.change(source + "/fork-batches", "create-live-generation",
        dict(children=[dict(name="live dependent")], require_independent=True), view))
    child, = batch["children"]
    assert child["state"] == "ready", child
    child_view, child_ref = platform.context(child["machine_id"])
    assert child_view["power_state"] == "running"
    volatile = rows(platform, """SELECT DISTINCT t.resource_id FROM fd_transfers t JOIN resources r USING(resource_id)
        JOIN fd_leases l USING(resource_id) WHERE r.durability='volatile' AND r.state='available'
        AND l.state='attached' AND l.incarnation_id=?""", (child_ref["incarnation_id"],))
    assert volatile, volatile
    old = [ref, child_ref]
    active_grants = []
    for execution in old:
        original, = rows(platform, """SELECT e.pid,e.pid_start_time,e.host_boot_id,g.state,g.grant_id FROM executions e
            JOIN writer_grants g USING(incarnation_id) WHERE e.incarnation_id=? AND g.state='attached'""",
                         (execution["incarnation_id"],))
        assert original[2:4] == (boot_id, "attached")
        active_grants.append(original[4])
        print("Active before host cut", execution["incarnation_id"], original)
    record = dict(old_boot=boot_id, volatile=[row[0] for row in volatile], old=old, active_grants=active_grants,
                  control=platform.epoch, port=platform.port,
                  bundle=platform.bundle_id, serve=platform.serve_id, sleeping=sleepy, saved_ref=saved_ref,
                  point=point, nonce=nonce, timer=timer, manifest_sha=digest(platform.bundle / "runtime-bundle.json"))
    file = platform.root / "observation"
    with open(file, "x", opener=lambda p, flags: os.open(p, flags, 0o600)) as out:
        json.dump(record, out)
    os.sync()
    print("Worker host", boot_id, "bundle", platform.bundle_id, "point", point["capture_id"],
          "leased volatile generations", record["volatile"], flush=True)


def next_boot(platform):
    record = json.loads((platform.root / "observation").read_text())
    boot_id = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    assert boot_id != record["old_boot"]
    root = platform.root
    subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
    subprocess.run(["mount", "--bind", root / "var-lib", "/var/lib"], check=True)
    # Recreate the production run directory in this boot. Only the Worker objects, registry and Serve DB persist.
    for path in (root / "runtime", root / "ipc", root / "api"):
        subprocess.run(["mount", "-t", "tmpfs", "-o", "mode=0755", "tmpfs", path], check=True)
    Path("/run/smolvm-custom").mkdir(exist_ok=True)
    subprocess.run(["mount", "--bind", root / "runtime", "/run/smolvm-custom"], check=True)
    platform.bundle = root / "bundle"
    subprocess.run(["mount", "--bind", platform.args.bundle, platform.bundle], check=True)
    platform.env = {**os.environ, "SMOLVM_LIB_DIR": str(platform.bundle / "lib"),
                    "LD_LIBRARY_PATH": str(platform.bundle / "lib"), "SMOLVM_GUEST_RELAY_DIR": str(platform.relay_dir)}
    assert digest(platform.bundle / "runtime-bundle.json") == record["manifest_sha"]
    lock = None
    if platform.args.hold_old_writer:
        owner = record["old"][0]["incarnation_id"]
        head, = rows(platform, "SELECT head_resource_id,backing_identity FROM writer_reservations WHERE incarnation_id=?",
                     (owner,))
        lock = platform.stack.enter_context((root / "catalog/objects" / head[0] / "data").open("r+b"))
        backing = os.fstat(lock.fileno())
        assert head[1] == f"inode:{backing.st_dev}:{backing.st_ino}", head
        fcntl.lockf(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    platform.broker = platform.spawn([platform.bundle / "smolvm-resourced", "serve", "--socket", platform.worker,
        "--state-dir", root / "catalog", "--socket-gid", str(grp.getgrnam("kvm").gr_gid)], "worker.log")
    health = wait_for(lambda: worker(platform.worker, "Health"))
    assert health["host_boot_id"] == boot_id, health
    for ident in record["volatile"]:
        assert rows(platform, "SELECT state FROM resources WHERE resource_id=? AND durability='volatile'",
                    (ident,)) == [("lost",)], ident
    if lock is not None:
        time.sleep(4)
        assert worker(platform.worker, "Health")["state"] == "recovering"
        assert rows(platform, "SELECT state FROM writer_grants WHERE grant_id=?",
                    (record["active_grants"][0],)) == [("unknown",)]
        assert rows(platform, "SELECT backing_identity FROM writer_reservations WHERE incarnation_id=?",
                    (owner,)) == [(head[1],)]
        fcntl.lockf(lock, fcntl.LOCK_UN)
        print("Unresolved ownership of", head[1], "kept the old writer reserved until the independent lock released", flush=True)
    if platform.args.automatic_recovery:
        wait_for(lambda: worker(platform.worker, "Health")["state"] == "ready", timeout=30)
    else:
        time.sleep(3)
        assert worker(platform.worker, "Health")["state"] == "recovering"
        for ref in record["old"]:
            phase, = rows(platform, "SELECT phase,host_boot_id FROM executions WHERE incarnation_id=?",
                          (ref["incarnation_id"],))
            assert phase == ("unknown", record["old_boot"]), phase
            result = worker(platform.worker, "FinalizeExecutionExit", route_id="deep", bundle_digest=record["bundle"],
                            expected=ref)
            assert result["phase"] == "exited" and result["incarnation_id"] == ref["incarnation_id"], result
        ready = worker(platform.worker, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]})
        assert ready["state"] == "ready"
    for ref in record["old"]:
        assert rows(platform, "SELECT phase FROM executions WHERE incarnation_id=?",
                    (ref["incarnation_id"],)) == [("exited",)]
        assert not rows(platform, "SELECT volume_id FROM writer_reservations WHERE incarnation_id=?", (ref["incarnation_id"],))
        assert not rows(platform, "SELECT 1 FROM writer_grants WHERE incarnation_id=? AND state<>'released'",
                        (ref["incarnation_id"],))
    for grant in record["active_grants"]:
        assert rows(platform, "SELECT state FROM writer_grants WHERE grant_id=?", (grant,)) == [("released",)]
    print("Catalog ready, previous-boot writers released, automatic recovery", platform.args.automatic_recovery,
          "new host", boot_id, flush=True)
    platform.serve_id, platform.bundle_id = record["serve"], record["bundle"]
    platform.spawn([platform.bundle / "smolvm-bin", "--state-dir", root / "state", "serve", "start",
                    "--listen", f"unix://{platform.serve}", "--worker-resource-socket", platform.worker,
                    "--guest-rollout-port", "37451", "--shutdown-grace", "0"], "serve.log")
    handshake = wait_for(lambda: platform.native("/capabilities"), retry=(RuntimeError, OSError))
    assert (handshake["runtime_instance_id"], handshake["bundle_digest"]) == (platform.serve_id, platform.bundle_id)
    platform.port = record["port"]
    platform.origin = f"http://127.0.0.1:{platform.port}"
    platform.spawn([platform.args.platformd, "--config", root / "platform.json"], "platform.log")
    secret = root / "reboot-owner-password"
    platform.login_owner(secret.read_text())
    secret.unlink()
    assert platform.epoch == record["control"]
    path = record["sleeping"]
    def saved_view():
        view = platform.app(path)
        return view if view["power_state"] == "hibernated" else None
    slot = {**record["saved_ref"], "incarnation_id": None}
    status = worker(platform.worker, "GetLifecycleStatus", route_id="deep", bundle_digest=platform.bundle_id,
                    expected=slot)
    assert (status["power"], status["statepoint"], status["incarnation_id"], status["execution_epoch"]) == (
        "HIBERNATED", record["point"]["capture_id"], None, slot["execution_epoch"]), status
    if platform.args.allow_stale_saved_view:
        view = platform.app(path)
        if view["power_state"] != "hibernated":
            assert view["power_state"] == "unknown" and "observation_stale" in view["blocked_reasons"], view
            assert view["binding"]["last_capture_id"] == status["statepoint"]
            print("Public machine observation is stale; live Worker confirms the original durable save", flush=True)
    else:
        view = wait_for(saved_view, timeout=45)
    assert rows(platform, "SELECT manifest_digest FROM statepoints WHERE capture_id=?", (record["point"]["capture_id"],)
                ) == [(record["point"]["manifest_digest"],)]
    platform.complete(platform.change(path + "/start", "durable-after-host-boot", {}, view))
    _, fresh = platform.context(path.split("/")[1])
    assert fresh["incarnation_id"] != record["saved_ref"]["incarnation_id"]
    assert fresh["execution_epoch"] == record["saved_ref"]["execution_epoch"] + 1
    if record.get("timer"):
        verify_timer(platform, fresh, record["timer"])
    for marker in ("/run/volatile-marker", "/root/durable-marker"):
        assert wait_for(lambda: platform.guest(fresh, ["cat", marker]), retry=(OSError, RuntimeError)) == record["nonce"].encode()
    print("Changed boot fenced", len(record["old"]), "executions; lost", len(record["volatile"]),
          "leased volatile generations; same durable point recovered", fresh, "RAM and disk nonce", record["nonce"], flush=True)


def guest(stage):
    assert "sandbox.release_fixture=isolated" in Path("/proc/cmdline").read_text().split()
    assert Path("/proc/1/comm").read_text().strip() == "systemd" and Path("/dev/kvm").is_char_device()
    assert subprocess.check_output(["findmnt", "-n", "-o", "SOURCE", "-T", SEED], text=True).strip() == "/dev/vdb"
    inputs = json.loads((SEED / "inputs.json").read_text())
    args = argparse.Namespace(bundle=SEED / inputs["bundle"], system_image=SEED / inputs["image"],
                              platformd=SEED / "platformd", automatic_recovery=inputs["automatic_recovery"],
                              allow_stale_saved_view=inputs["allow_stale_saved_view"], hold_old_writer=inputs["hold_old_writer"],
                              clock_probe=SEED / "clock-probe" if inputs["clock_probe"] else None)
    os.environ["SMOLVM_WORKER_SLICE_NAME"] = "smolvmworkertest.slice"
    subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
        os.environ["SMOLVM_WORKER_SLICE_NAME"], "fail", "1", "MemoryAccounting", "b", "true", "0"], check=True)
    ROOT.mkdir(mode=0o755, exist_ok=True)
    with ExitStack() as stack:
        platform = Platform(args, ROOT, stack, use_installation=True)
        try:
            if stage == "running":
                first_boot(platform)
            else:
                next_boot(platform)
            boot_id = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            print("RELEASE_FIXTURE_OK", stage, boot_id, flush=True)
            if stage == "running" and inputs["orderly_shutdown"]:
                subprocess.run(["systemctl", "--no-block", "poweroff"], check=True)
            signal.pause()
        except BaseException:
            for log in ("worker.log", "serve.log", "platform.log"):
                path = ROOT / log
                if path.exists():
                    print(log, path.read_text(errors="replace")[-3500:], file=sys.stderr, flush=True)
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image", "base-image", "kernel"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--kernel-release", default=os.uname().release)
    parser.add_argument("--kvm-module", choices=("kvm_amd", "kvm_intel"), required=True)
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--qemu-img", default="qemu-img")
    parser.add_argument("--firmware-dir")
    parser.add_argument("--bios")
    parser.add_argument("--work-dir", type=Path)
    parser.add_argument("--timeout", type=int, default=480)
    parser.add_argument("--host-memory-mib", type=int, default=2304, help="RAM assigned to the disposable Worker host")
    parser.add_argument("--automatic-recovery", action="store_true",
                        help="require the Worker to fence prior-boot managed executions and become ready without admin RPCs")
    parser.add_argument("--allow-stale-saved-view", action="store_true",
                        help="measure an older platform which ages out a retained manual Hibernate observation")
    parser.add_argument("--orderly-shutdown", action="store_true",
                        help="let the guest systemd power off cleanly before booting its disk again")
    parser.add_argument("--hold-old-writer", action="store_true",
                        help="hold one original writable backing locked in the new boot before starting the Worker")
    parser.add_argument("--standby-clock-probe", type=lambda path: Path(path).resolve(strict=True),
                        help="also verify a live Guest timer across managed Standby, Hibernate and the new host boot using static standby-clock.c")
    args = parser.parse_args()
    if args.hold_old_writer and not args.automatic_recovery:
        parser.error("--hold-old-writer requires --automatic-recovery")
    here = Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="worker-reboot-") as folder:
        scratch = Path(folder)
        inputs = scratch / "input"
        inputs.mkdir()
        collect_modules(args, inputs)
        shutil.copyfile(here / "worker_host_guest.sh", inputs / "guest.sh")
        shutil.copy2(args.platformd, inputs / "platformd")
        if args.standby_clock_probe:
            shutil.copy2(args.standby_clock_probe, inputs / "clock-probe")
        for script in (Path(__file__), here / "managed_system_vsock.py", here / "release_host_reboot.py"):
            shutil.copy2(script, inputs)
        (inputs / "inputs.json").write_text(json.dumps(dict(bundle=args.bundle.name, image=args.system_image.name,
            automatic_recovery=args.automatic_recovery, allow_stale_saved_view=args.allow_stale_saved_view,
            orderly_shutdown=args.orderly_shutdown, hold_old_writer=args.hold_old_writer, clock_probe=bool(args.standby_clock_probe))))
        seed = scratch / "inputs.sqfs"
        subprocess.run(["mksquashfs", *sorted(inputs.iterdir()), args.bundle, args.system_image, seed,
                        "-noappend", "-processors", "2", "-comp", "lz4", "-no-progress", "-quiet"], check=True)
        disk = scratch / "host.qcow2"
        subprocess.run([args.qemu_img, "create", "-f", "qcow2", "-F", "raw", "-b", args.base_image, disk, "9G"], check=True)
        before = boot(args, seed, disk, "running", memory_mib=args.host_memory_mib, orderly_shutdown=args.orderly_shutdown)
        after = boot(args, seed, disk, "recovered", memory_mib=args.host_memory_mib)
        assert before != after
        mode = "orderly shutdown" if args.orderly_shutdown else "virtual power cut"
        print("Worker host", mode, "with two live VMMs and a durable save", before, "->", after, flush=True)


if __name__ == "__main__":
    if sys.argv[1:] in (["--guest", "running"], ["--guest", "recovered"]):
        guest(sys.argv[2])
    else:
        main()
