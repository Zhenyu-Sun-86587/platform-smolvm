#!/usr/bin/env python3
"""Send owner API and HTTP service requests across an actual Hibernate stop."""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, closing
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import sys
import tempfile
from threading import Barrier, Timer

from authorized_services_kvm import NETWORK, PROGRAM, RECEIPTS, SERVICE_PORT, create_service, machine, tls_setup, worker_get, write_service
from managed_system_vsock import Platform, wait_for


def verify(args, root, stack):
    _, tls, gateway = tls_setup(root)
    platform = Platform(args, root, stack, (NETWORK,), state_tmpfs=True, use_installation=True, tls=tls, gateway=gateway)
    platform.start()
    mid, _, source = machine(platform, "access-cut", "native")
    path = "machines/" + mid
    policy = platform.app(path + "/lifecycle")
    platform.complete(platform.change(path, "cut-owner-policy", {
        "expected_policy_revision": policy["desired_revision"],
        "lifecycle_policy": {"auto_standby": False, "auto_hibernate": False,
                             "wake_conditions": {"api_access": True, "service_access": True}},
    }, platform.app(path), method="PATCH"))
    proof = os.urandom(12).hex()
    platform.guest(source, ["sh", "-ec", 'printf %s "$1" >/run/access-cut-memory', "marker", proof])
    write_service(platform, source)
    platform.guest(source, ["systemd-run", "--unit=cut-service", "/usr/bin/perl", PROGRAM, "http", str(SERVICE_PORT)])
    wait_for(lambda: f":{SERVICE_PORT:04X}".encode() in platform.guest(source, ["cat", "/proc/net/tcp"]))
    service_id = create_service(platform, mid, "http", SERVICE_PORT)
    service = platform.app("services/" + service_id)
    credential = platform.change("tokens", "cut-http", dict(label="Cut HTTP",
                                 scope=dict(access="tunnel", machine_id=mid)), status=200)

    if args.original == "STANDBY":
        platform.complete(platform.change(path + "/standby", "cut-standby", {}, platform.app(path)))
        assert worker_get(platform, "GetLifecycleStatus", source)["power"] == args.original

    catalog = stack.enter_context(closing(sqlite3.connect((root / "catalog/catalog.sqlite").as_uri() + "?mode=ro", uri=True)))

    def one(sql, *args):
        return catalog.execute(sql, args).fetchone()

    baseline, = one("SELECT coalesce(max(rowid),0) FROM lifecycle_protections")
    events = worker_get(platform, "QueryLifecycleEvents", source, {"after": 0})["events"]
    cursor = max((event["seq"] for event in events), default=0)
    accepted = platform.change(path + "/hibernate", "cut-hibernate", {}, platform.app(path))

    def cut():
        row = one("""SELECT b.operation_id,b.original,b.stage,p.phase,b.capture_id
                     FROM managed_fork_batches b JOIN operations o USING(operation_id)
                     LEFT JOIN statepoints p USING(capture_id)
                     WHERE b.source_incarnation=? AND o.kind='hibernate'""", source["incarnation_id"])
        if row and (args.side == "before" or row[3] in ("stop_committed", "hibernated")):
            return row

    operation, original, stage, point, capture = wait_for(cut, timeout=100)
    assert original == args.original and ((point in ("stop_committed", "hibernated")) == (args.side == "after"))
    print("Hibernate boundary", json.dumps(dict(original=original, stage=stage, point=point, capture=capture)), flush=True)
    rendezvous = Barrier(2, timeout=15)
    business = "http-" + proof
    if args.pause_worker:
        # A fully saved source isolates a slow control reply from the stop decision.
        wait_for(lambda: platform.native("/operations/" + operation)["phase"] == "COMPLETED")
        wait_for(lambda: platform.app(path)["power_state"] == "hibernated")
        platform.broker.send_signal(signal.SIGSTOP)
        timer = Timer(6.5, platform.broker.send_signal, args=(signal.SIGCONT,))
        timer.start()
        stack.callback(timer.join)
        stack.callback(platform.broker.send_signal, signal.SIGCONT)

    def public_http():
        rendezvous.wait()
        with closing(platform.api_connection(timeout=105)) as client:
            client.request("POST", "/receipt/" + business, body=b"", headers={
                "Host": service["public_host"], "Content-Length": "0", "Authorization": "Bearer " + credential["token"],
            })
            reply = client.getresponse()
            return reply.status, reply.read()

    def public_exec():
        rendezvous.wait()
        script = 'printf "%s\\n" "$1" >>/run/access-cut-api; cat /run/access-cut-memory'
        return platform.app(path + "/execs", "POST", dict(argv=["/bin/sh", "-ec", script, "receipt", proof],
                            cwd="/", wait_seconds=15), {"If-Control-Epoch": platform.epoch,
                            "Idempotency-Key": "cut-owner-exec"}, timeout=105)

    with ThreadPoolExecutor(max_workers=2) as clients:
        http = clients.submit(public_http)
        api = clients.submit(public_exec)
        if args.withdraw_service:
            wait_for(lambda: one("""SELECT 1 FROM lifecycle_protections WHERE rowid>? AND
                                     reason='service_access' AND kind='wake' AND state='pending'""", baseline), timeout=15)
            platform.app("tokens/" + credential["token_id"], "DELETE", {}, {
                "If-Control-Epoch": platform.epoch, "If-Match": f'"{credential["revision"]}"',
                "Idempotency-Key": "owner-withdraw-pending-service",
            })
        status, reply = http.result()
        result = api.result()

    if args.withdraw_service:
        assert status in (401, 403), (status, reply)
    else:
        assert (status, reply) == (200, (business + "\n").encode()), (status, reply)
    finished = wait_for(lambda: row if (row := platform.app("execs/" + result["exec_id"]))["state"] == "exited" else None)
    assert finished["exit_code"] == 0, finished
    active = platform.context(mid)[1]
    assert active["instance_id"] == source["instance_id"]
    assert active["execution_epoch"] == source["execution_epoch"] + int(args.side == "after")
    assert (active["incarnation_id"] == source["incarnation_id"]) == (args.side == "before")
    api_receipts = platform.guest(active, ["cat", "/run/access-cut-api"]).decode().splitlines()
    http_receipts = platform.guest(active, ["sh", "-c", 'if test -f "$1"; then cat "$1"; fi', "receipts", RECEIPTS]).decode().splitlines()
    assert api_receipts == [proof] and Counter(http_receipts) == Counter([] if args.withdraw_service else [business])
    assert platform.guest(active, ["cat", "/run/access-cut-memory"]).decode() == proof

    def terminal():
        native = platform.native("/operations/" + operation)
        return native if native["phase"] in ("ABORTED", "COMPLETED") else None

    native = wait_for(terminal)
    assert native["phase"] == ("ABORTED" if args.side == "before" else "COMPLETED"), native
    def settled_public():
        row = platform.app("operations/" + accepted["operation_id"])
        return row if row["state"] in ("failed", "cancelled", "succeeded") else None

    final = wait_for(settled_public)
    assert (final["state"] == "succeeded") == (args.side == "after"), final
    events = worker_get(platform, "QueryLifecycleEvents", active, {"after": cursor})["events"]
    restorations = {e["operation_id"] for e in events if e["kind"] == "vm_restore" and e["origin"] == "wake"}
    assert len(restorations) == int(args.side == "after"), events
    if restorations:
        restore = platform.native("/operations/" + next(iter(restorations)))
        assert restore["statepoint"]["capture_id"] == capture and restore["statepoint"]["stage"] == "released"
    rows = catalog.execute("""SELECT kind,state,incarnation_id,epoch,wake_operation FROM lifecycle_protections
                              WHERE rowid>? AND id LIKE 'access-%' ORDER BY rowid""", (baseline,)).fetchall()
    admitted = [r for r in rows if r[0] == "hold" and r[2:4] == (active["incarnation_id"], active["execution_epoch"])]
    assert len(admitted) == (1 if args.withdraw_service else 2), rows
    if args.side == "after":
        assert {r[4] for r in admitted} == restorations, rows
    print(json.dumps(dict(bundle=platform.bundle_id, original=original, side=args.side, native=native["phase"],
                          epoch=active["execution_epoch"], capture=capture, restorations=sorted(restorations),
                          activation_holds=len(admitted), api_receipts=api_receipts, http_receipts=http_receipts,
                          http_status=status, withdrawn=args.withdraw_service)), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(option, type=lambda p: Path(p).resolve(strict=True), required=True)
    parser.add_argument("--original", choices=("RUNNING", "STANDBY"), required=True)
    parser.add_argument("--side", choices=("before", "after"), required=True)
    parser.add_argument("--withdraw-service", action="store_true",
                        help="owner withdraws one already pending request during exact restore")
    parser.add_argument("--pause-worker", action="store_true", help="delay the control reply across one RPC timeout")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--local", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.withdraw_service and args.side != "after":
        parser.error("withdrawal requires an exact restore in progress")
    if args.pause_worker and args.side != "after":
        parser.error("the control transport delay requires a fully committed source")
    if not args.local:
        subprocess.run(["unshare", "--mount", "--propagation", "private", "--net", "--",
                        sys.executable, "-B", __file__, *sys.argv[1:], "--local"], check=True)
        return
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="access-cut-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=3G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmcutapi{os.getpid()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"], stdout=subprocess.DEVNULL, check=True)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        try:
            verify(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3200:], file=sys.stderr)
            catalog = root / "catalog/catalog.sqlite"
            if catalog.exists():
                with closing(sqlite3.connect(catalog.as_uri() + "?mode=ro", uri=True)) as db:
                    for label, sql in (
                        ("operations", "SELECT kind,phase FROM operations"),
                        ("decisions", "SELECT kind,origin,stage FROM lifecycle_decisions"),
                        ("protections", "SELECT kind,state,reason,epoch FROM lifecycle_protections"),
                        ("captures", "SELECT original,stage,source_released FROM managed_fork_batches"),
                    ):
                        print(label, db.execute(sql + " ORDER BY rowid DESC LIMIT 8").fetchall(), file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
