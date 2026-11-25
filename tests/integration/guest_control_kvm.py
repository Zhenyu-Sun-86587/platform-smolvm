#!/usr/bin/env python3
"""Exercise delegated Guest mutations and a host-only TLS credential in KVM."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, closing
from datetime import datetime, timedelta, timezone
from http.client import HTTPConnection, HTTPResponse, IncompleteRead
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import ssl
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time

from managed_system_vsock import Platform, ResponseBytes, request, wait_for, worker


class Upstream:
    def __init__(self, root, stack):
        self.key = "fixture-" + os.urandom(24).hex()
        self.started = threading.Event()
        self.release = threading.Event()
        self.requests = []
        secret = root / "provider-key"
        secret.touch(mode=0o600)
        secret.write_text(self.key + "\n")
        cert, key_file = root / "provider.pem", root / "provider-private.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
                        "-keyout", key_file, "-out", cert], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
        upstream = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                upstream.requests.append((self.path, self.headers.get("Authorization") == "Bearer " + upstream.key,
                                          self.headers.get("Proxy-Authorization") is None))
                if self.path == "/redirect":
                    self.send_response(307)
                    self.send_header("Location", "/outside-policy")
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("X-Fixture-Must-Stay-Host", upstream.key)
                self.end_headers()
                try:
                    if self.path == "/inspect":
                        payload = upstream.key.encode()
                        self.wfile.write(b"data: " + payload[:13])
                        self.wfile.flush()
                        time.sleep(0.05)
                        self.wfile.write(payload[13:] + b"\n\n")
                    else:
                        self.wfile.write(b"data: waiting\n\n")
                        self.wfile.flush()
                        upstream.started.set()
                        if upstream.release.wait(18):
                            self.wfile.write(b"data: complete\n\n")
                except (ConnectionError, ssl.SSLError):
                    pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.daemon_threads = True
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key_file)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        stack.callback(server.server_close)
        stack.callback(server.shutdown)
        stack.callback(self.release.set)
        self.policy = dict(id="model", upstream=f"https://127.0.0.1:{server.server_port}",
                           credential_file=str(secret), ca_file=str(cert), allow_private=True, timeout_seconds=35,
                           routes=[dict(method="GET", path=path) for path in ("/inspect", "/redirect", "/stream")])


def wire(platform, ref, probe, method, path, token, body=None, **more):
    raw = b"" if body is None else json.dumps(body).encode()
    headers = {"Host": "guest-relay", "Authorization": "Bearer " + token, "Connection": "close",
               "Content-Length": str(len(raw)), "Content-Type": "application/json", **more}
    text = f"{method} /v1/{path} HTTP/1.1\r\n" + "".join(f"{key}: {value}\r\n" for key, value in headers.items()) + "\r\n"
    result = platform.guest(ref, [probe], text + raw.decode())
    response = HTTPResponse(ResponseBytes(result))
    response.begin()
    try:
        payload = response.read()
    except IncompleteRead as error:
        payload = error.partial
    return response.status, payload, response.headers


def credential(platform, machine, action, label, **fields):
    created = platform.change("guest-grants", label + "-grant", dict(source_machine_id=machine, action=action,
        expires_at=(datetime.now(timezone.utc)+timedelta(minutes=8)).isoformat(), maximum_uses=20, **fields), status=200)
    grant = created["grant_id"]
    token = platform.change(f"guest-grants/{grant}/credential", label + "-token", {"ttl_seconds": 420}, status=200)["token"]
    return grant, token


def mutation(platform, ref, probe, route, token, key, value, machine=None, status=202):
    headers = {"If-Control-Epoch": platform.epoch, "Idempotency-Key": key}
    if machine is not None:
        headers["If-Match"] = f'"{machine["revision"]}"'
        value = {**value, "expected_generation": machine["binding_generation"]}
    actual, body, _ = wire(platform, ref, probe, "POST", route, token, value, **headers)
    assert actual == status, (route, actual, body)
    return json.loads(body)


def delegated(platform, machine, ref, probe):
    _, create = credential(platform, machine, "machine.create", "delegated-create", maximum_concurrent=1, limits=dict(
        max_children=1, max_total_memory_mib=640, create=dict(template_id="t_system", runtime_instance_id=platform.serve_id,
                                                               cpu_count=1, memory_mib=640)))
    assert wire(platform, ref, probe, "POST", "machines", create, {"name":"reject template", "template_id":"foreign"})[0] == 400
    accepted = mutation(platform, ref, probe, "machines", create, "approved-create", {"name":"Guest-created System"})
    status, control, _ = wire(platform, ref, probe, "GET", "control", create)
    assert status == 200 and json.loads(control) == {"control_epoch": platform.epoch}
    result = platform.complete(accepted)
    _, created_ref = platform.context(result["machine_id"])
    assert wait_for(lambda: platform.guest(created_ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
                    retry=(OSError, RuntimeError))
    assert mutation(platform, ref, probe, "machines", create, "quota-create", {"name":"Over delegated memory"}, status=429)["error"]["code"] == "RESOURCE_LIMIT"
    assert mutation(platform, ref, probe, "machines", create, "approved-create", {"name":"Guest-created System"})["operation_id"] == accepted["operation_id"]
    assert wire(platform, ref, probe, "GET", "operations/" + accepted["operation_id"], create)[0] == 200
    request(HTTPConnection("127.0.0.1", platform.port, timeout=10), "GET", "/v1/operations/" + accepted["operation_id"],
            headers={"Authorization": "Bearer " + create}, status=401)

    _, fork_token = credential(platform, machine, "machine.fork", "delegated-fork", target_machine_id=machine,
                         limits=dict(max_children=2, max_total_memory_mib=1408))
    denied = wire(platform, ref, probe, "POST", "machines/" + result["machine_id"] + "/forks", fork_token, {"child_name":"outside scope"})
    assert denied[0] == 403
    path = "machines/" + machine
    source = wait_for(lambda: (candidate := platform.app(path))["capabilities"]["state.independent_fork"]["supported"] and candidate)
    fork = mutation(platform, ref, probe, path + "/forks", fork_token, "approved-fork", {"child_name":"Guest-forked System"}, source)
    try:
        forked = platform.complete(fork)
    except RuntimeError as error:
        with closing(sqlite3.connect(platform.root / "catalog/catalog.sqlite")) as db:
            rows = db.execute("SELECT stage,error FROM managed_fork_batches WHERE operation_id=?", (fork["operation_id"],)).fetchall()
        raise RuntimeError((error, rows)) from error
    (child,) = forked["children"]
    _, child_ref = platform.context(child["machine_id"])
    assert platform.guest(child_ref, [probe, "--cid"]) != platform.guest(ref, [probe, "--cid"])
    assert wire(platform, child_ref, probe, "GET", "operations/" + forked["operation_id"], fork_token)[0] == 401
    with closing(sqlite3.connect(platform.root / "platform/platform.db")) as db:
        rows = db.execute("SELECT creator_principal_id,owner_principal_id,reserved_memory_mib FROM guest_created_machines ORDER BY reserved_memory_mib").fetchall()
        assert len(rows) == 2 and rows[0][:2] == rows[1][:2] and [row[2] for row in rows] == [640,768], rows
    print("Guest vsock created and forked actual System VMs within an owner grant. The child and public endpoint rejected the inherited bearer.", flush=True)


def proxy(platform, upstream, machine, ref, probe):
    grant, key = credential(platform, machine, "credential_proxy.use", "model", proxy_id="model", maximum_concurrent=1)
    base = "credential-proxies/model/"
    extra = {"Proxy-Authorization":"Guest-auth-must-not-forward"}
    status, sample, headers = wire(platform, ref, probe, "GET", base + "inspect", key, **extra)
    assert status == 200 and upstream.key.encode() not in sample and b"[redacted]" in sample and "X-Fixture-Must-Stay-Host" not in headers
    assert wire(platform, ref, probe, "GET", base + "redirect", key)[0] == 502
    assert wire(platform, ref, probe, "GET", base + "outside-policy", key)[0] == 403
    assert upstream.requests == [("/inspect", True, True), ("/redirect", True, True)], upstream.requests

    catalog = platform.root / "catalog/catalog.sqlite"

    def hold_state():
        with closing(sqlite3.connect(catalog)) as db:
            return {row[0] for row in db.execute("SELECT state FROM lifecycle_protections WHERE principal=?", (grant,))}

    def lifecycle_status():
        return worker(platform.worker, "GetLifecycleStatus", route_id="deep", bundle_digest=platform.bundle_id, expected=ref)

    # The activity collector checks the installed task daemon against the
    # actual Platform binding before it can report zero unmanaged activity.
    with closing(sqlite3.connect(platform.root / "platform/platform.db")) as db:
        binding, generation, namespace = db.execute("SELECT b.id,b.generation,b.task_namespace FROM machines m "
            "JOIN runtime_bindings b ON b.id=m.current_binding_id WHERE m.id=?", (machine,)).fetchone()
    task_scope = dict(machine_id=machine, generation=generation, task_namespace=namespace, nonce=binding)
    bound = json.loads(platform.guest(ref, ["/usr/local/bin/taskctl", "rpc"], json.dumps(dict(method="bind", binding=task_scope))))
    assert bound["binding"] == task_scope, bound

    def launch_stream(pool):
        upstream.release.clear()
        upstream.started.clear()
        task = pool.submit(wire, platform, ref, probe, "GET", base + "stream", key)
        assert upstream.started.wait(20), "proxy request did not reach the fixture model"
        assert "active" in hold_state(), hold_state()
        return task

    with ThreadPoolExecutor(max_workers=2) as pool:
        waiting = launch_stream(pool)
        machine_path = "machines/" + machine
        policy = platform.app(machine_path + "/lifecycle")
        assert policy["supported"], policy
        platform.complete(platform.change(machine_path, "idle-while-streaming", dict(expected_policy_revision=policy["desired_revision"],
            lifecycle_policy=dict(auto_standby=True, standby_after_seconds=2, auto_hibernate=False, min_running_after_wake_seconds=0)),
            platform.app(machine_path), method="PATCH"))
        assert wire(platform, ref, probe, "GET", base + "inspect", key)[0] == 429
        time.sleep(5)
        assert not waiting.done() and "active" in hold_state()
        status = lifecycle_status()
        assert status["power"] == "RUNNING" and "hold:credential_proxy" in status["blockers"], status
        print("Worker blockers during model wait", status["blockers"], flush=True)
        upstream.release.set()
        status, output, _ = waiting.result(timeout=15)
        assert status == 200 and b"data: waiting" in output and b"data: complete" in output
        wait_for(lambda: "active" not in hold_state() and "released" in hold_state(), timeout=10)
        print("A29 model wait held the low-CPU Guest beyond its two-second automatic standby timeout and released the hold after the second SSE event.", flush=True)

        revoked = launch_stream(pool)
        time.sleep(0.5)
        platform.app(f"guest-grants/{grant}", "DELETE", {}, {
            "If-Control-Epoch": platform.epoch, "Idempotency-Key":"model-revoke", "If-Match":'"1"',
        })
        status, output, _ = revoked.result(timeout=10)
        upstream.release.set()
        assert status == 200 and b"data: waiting" in output and b"data: complete" not in output
        wait_for(lambda: "active" not in hold_state(), timeout=10)
        assert wire(platform, ref, probe, "GET", base + "inspect", key)[0] == 401
        for path in (platform.root / "platform.log", platform.root / "platform/platform.db",
                     platform.root / "platform/platform.db-wal"):
            if path.exists():
                assert upstream.key.encode() not in path.read_bytes(), path.name
        print("Revocation closed the current Guest stream before the second event. The host-only key and cross-path redirect stayed inside the proxy boundary.", flush=True)
        try:
            wait_for(lambda: platform.native(f"/instances/{ref['instance_id']}/inventory")["inventory"]["source_disposition"] == "STANDBY", timeout=45)
        except (RuntimeError, TimeoutError) as error:
            raise RuntimeError((error, lifecycle_status())) from error
        print("The same Worker placed the Guest in automatic standby after the last stream hold was released.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for field in ("bundle", "platformd", "system-image", "probe"):
        parser.add_argument("--" + field, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--case", choices=("delegate", "proxy"), required=True)
    parser.add_argument("--local-child", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if not args.local_child:
        subprocess.run(["unshare", "--mount", "--propagation", "private", "--net", "--", sys.executable,
                        __file__, *sys.argv[1:], "--local-child"], check=True)
        return
    with tempfile.TemporaryDirectory(prefix="gcred-kvm-", dir="/var/tmp") as tmp, ExitStack() as stack:
        root = Path(tmp)
        root.chmod(0o755)
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
        upstream = Upstream(root, stack) if args.case == "proxy" else None
        platform = Platform(args, root, stack, credential_proxies=[upstream.policy] if upstream else ())
        try:
            platform.start()
            source = platform.change("machines", "source", dict(name="Guest authority source", template_id="t_system",
                boot_mode="system", runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=2,memory_mib=768)))
            platform.complete(source)
            machine = source["machine_id"]
            _, ref = platform.context(machine)
            wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
            probe = platform.place_probe(ref, args.probe)
            if upstream:
                proxy(platform, upstream, machine, ref, probe)
            else:
                delegated(platform, machine, ref, probe)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2400:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
