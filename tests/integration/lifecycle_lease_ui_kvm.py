#!/usr/bin/env python3
"""Check the owner's lease states in Chromium against a real System guest."""

import argparse
from contextlib import ExitStack, contextmanager
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from urllib.request import urlopen

from managed_system_vsock import Platform, wait_for


@contextmanager
def browser(args, platform, path):
    from websockets.sync.client import connect

    profile = platform.root / "chromium"
    env = os.environ.copy()
    if args.chromium_library_dir:
        env["LD_LIBRARY_PATH"] = str(args.chromium_library_dir)
    with (platform.root / "chromium.log").open("wb") as log:
        chrome = subprocess.Popen([args.chromium, "--headless", "--no-sandbox", "--disable-gpu",
                                   "--disable-dev-shm-usage", "--remote-debugging-port=0",
                                   f"--user-data-dir={profile}", "about:blank"], env=env, stdout=log, stderr=log)
        try:
            port = wait_for(lambda: (profile / "DevToolsActivePort").read_text().splitlines()[0], timeout=20)
            with urlopen(f"http://127.0.0.1:{port}/json/list", timeout=10) as response:
                page = next(item for item in json.load(response) if item["type"] == "page")
            with connect(page["webSocketDebuggerUrl"], open_timeout=10) as connection:
                seq = 0

                def send(method, **params):
                    nonlocal seq
                    seq += 1
                    connection.send(json.dumps(dict(id=seq, method=method, params=params)))
                    while True:
                        reply = json.loads(connection.recv(timeout=20))
                        if reply.get("id") == seq:
                            assert "error" not in reply, reply
                            return reply["result"]

                name, value = platform.cookie.split("=", 1)
                assert send("Network.setCookie", name=name, value=value, url=platform.origin,
                            path="/v1", httpOnly=True)["success"]
                send("Page.navigate", url=f"{platform.origin}/#/{path}/activity")

                def read(expression):
                    result = send("Runtime.evaluate", expression=expression, returnByValue=True)
                    assert "exceptionDetails" not in result, result
                    return result["result"].get("value")

                yield read
        finally:
            chrome.terminate()
            chrome.wait(timeout=15)


def verify(args, platform):
    platform.start()
    created = platform.change("machines", "lease-console-system", dict(
        name="Lease status", template_id="t_system", boot_mode="system", start=True,
        runtime_instance_id=platform.serve_id, resources=dict(cpu_count=1, memory_mib=512),
        lifecycle_policy=dict(auto_standby=True, standby_after_seconds=1800, auto_hibernate=False),
    ))
    platform.complete(created)
    machine = created["machine_id"]
    path = "machines/" + machine
    _, execution = platform.context(machine)
    wait_for(lambda: platform.guest(execution, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(OSError, RuntimeError))

    def acquire(version):
        view = platform.app(path)
        return platform.app(path + "/leases", "POST",
                            dict(reason=f"Owner lease v{version}", semantics_version=version, ttl_seconds=3),
                            {"If-Match": f'"{view["revision"]}"', "If-Control-Epoch": platform.epoch,
                             "Idempotency-Key": f"lease-ttl-{version}"})

    legacy, bounded = acquire(1), acquire(2)

    def observed(identifier, state):
        lease = next(item for item in platform.app(path + "/leases")["items"] if item["lease_id"] == identifier)
        return lease if lease["state"] == state else None

    overdue = wait_for(lambda: observed(legacy["lease_id"], "suspect"))
    expired = wait_for(lambda: observed(bounded["lease_id"], "released"))
    assert overdue["release_reason"] == "renewal overdue" and expired["release_reason"] == "expired"
    platform.guest(execution, ["/bin/systemctl", "stop", "sandbox-taskd.service"])

    def unknown_task():
        policy = platform.app(path + "/lifecycle")
        return policy if "guest:unknown:task" in policy.get("effective_blockers", []) else None

    blocked = wait_for(unknown_task)
    with browser(args, platform, path) as read:
        def displayed():
            rows = read("""Array.from(document.querySelectorAll('section .rows .row'), r => ({
                id: r.querySelector('code')?.textContent, state: r.querySelector('.state')?.textContent,
                description: r.querySelector('small')?.textContent,
                actions: Array.from(r.querySelectorAll('button'), b => [b.textContent, b.disabled])
            }))""")
            return {row["id"]: row for row in rows if row.get("id")} if len(rows) >= 2 else None

        rows = wait_for(displayed)
        old, short = rows[legacy["lease_id"]], rows[bounded["lease_id"]]
        assert old["state"] == "Suspect" and "still protects" in old["description"]
        assert short["state"] == "Expired" and "protection ended" in short["description"]
        assert old["actions"] == [["Renew", False], ["Release", False]]
        assert short["actions"] == [["Renew", True], ["Release", True]]
        message = "Automatic sleep is blocked while guest / task status is unknown."
        assert wait_for(lambda: message in read("document.body.innerText"))

    def update(method, key, row, ttl=None):
        body = {"lease_token": legacy["lease_token"]}
        if ttl is not None:
            body["ttl_seconds"] = ttl
        return platform.app("leases/" + legacy["lease_id"], method, body, {
            "Idempotency-Key": key, "If-Match": f'"{row["revision"]}"', "If-Control-Epoch": platform.epoch,
        })

    active = update("PATCH", "renew-own-lease", overdue, 60)
    assert active["state"] == "active" and active["revision"] > overdue["revision"]
    released = update("DELETE", "release-own-lease", active)
    assert released["state"] == "released"
    assert platform.context(machine)[1] == execution
    print(json.dumps(dict(bundle=platform.bundle_id, epoch=execution["execution_epoch"],
                          browser_states=[old, short], unknown=message, blockers=blocked["effective_blockers"],
                          owner_renewal=active["state"], owner_release=released["state"])), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("--bundle", "--platformd", "--system-image", "--chromium"):
        parser.add_argument(option, type=lambda p: Path(p).resolve(strict=True), required=True)
    parser.add_argument("--chromium-library-dir", type=Path)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--local", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not args.local:
        subprocess.run(["unshare", "--mount", "--propagation", "private", "--net", "--",
                        sys.executable, "-B", __file__, *sys.argv[1:], "--local"], check=True)
        return
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="lease-ui-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=2G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmlease{os.getpid()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"], stdout=subprocess.DEVNULL, check=True)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        verify(args, Platform(args, root, stack, state_tmpfs=True, use_installation=True))


if __name__ == "__main__":
    main()
