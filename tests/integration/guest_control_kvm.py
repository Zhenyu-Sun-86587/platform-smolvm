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
    def __init__(self, root, stack, *, fork_stream=False):
        self.key = "fixture-" + os.urandom(24).hex()
        self.started = threading.Event()
        self.release = threading.Event()
        self.requests = []
        self.answers = {"/answer/" + name: os.urandom(12).hex() for name in ("source", "first", "second")} if fork_stream else {}
        self.fork_head = os.urandom(12).hex()
        self.fork_tail = os.urandom(12).hex()
        self.model_request = json.dumps({"stream": True, "prompt": os.urandom(12).hex()})
        self.model_posts = []
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
                self.serve_stream()

            def do_POST(self):
                if self.path != "/fork-stream":
                    self.send_error(404)
                    return
                length = int(self.headers.get("Content-Length", 0))
                if not 0 < length <= 4096:
                    self.send_error(400)
                    return
                upstream.model_posts.append(self.rfile.read(length))
                self.serve_stream()

            def serve_stream(self):
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
                    if self.path in upstream.answers:
                        self.wfile.write(f"data: {upstream.answers[self.path]}\n\n".encode())
                    elif self.path == "/fork-stream":
                        self.wfile.write(f"data: {upstream.fork_head}\n\n".encode())
                        self.wfile.flush()
                        upstream.started.set()
                        if upstream.release.wait(90):
                            self.wfile.write(f"data: {upstream.fork_tail}\n\n".encode())
                    elif self.path == "/inspect":
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
        routes = ("/fork-stream", *self.answers) if fork_stream else ("/inspect", "/redirect", "/stream")
        self.policy = dict(id="model", upstream=f"https://127.0.0.1:{server.server_port}",
                           credential_file=str(secret), ca_file=str(cert), allow_private=True,
                           timeout_seconds=100 if fork_stream else 35,
                           routes=[dict(method="POST" if path == "/fork-stream" else "GET", path=path) for path in routes])


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
    child_relay = platform.relay_dir / (child_ref["incarnation_id"] + ".sock")
    wait_for(child_relay.is_socket, timeout=15)
    response = wait_for(lambda: wire(platform, child_ref, probe, "GET", "operations/" + forked["operation_id"], fork_token),
                        timeout=15, retry=(OSError, RuntimeError))
    assert response[0] == 401, response[0]
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
    wait_for(lambda: not lifecycle_status()["blockers"], timeout=30)

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
        assert status["power"] == "RUNNING" and status["blockers"] == ["hold:credential_proxy"], status
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


def protected_stream_fork(platform, upstream, machine, ref, probe):
    source = "machines/" + machine
    policy = dict(auto_standby=False, auto_hibernate=False, keep_running=True)
    settings = platform.app(source + "/lifecycle")
    platform.complete(platform.change(source, "protect-source", dict(expected_policy_revision=settings["desired_revision"],
        lifecycle_policy=policy), platform.app(source), method="PATCH"))
    lease = platform.app(source + "/leases", "POST", dict(reason="waiting for model", semantics_version=2, ttl_seconds=240),
        {"If-Control-Epoch": platform.epoch, "If-Match": f'"{platform.app(source)["revision"]}"', "Idempotency-Key": "keep-model-running"})
    assert lease["state"] == "active"
    grant, token = credential(platform, machine, "credential_proxy.use", "first-stream", proxy_id="model")
    folder = "/run/protected-model"
    platform.guest(ref, ["mkdir", "-m", "700", folder])

    def write_request(execution, filename, route, bearer, body=None):
        method = "GET" if body is None else "POST"
        entity = "" if body is None else f"Content-Type: application/json\r\nContent-Length: {len(body.encode())}\r\n"
        message = (f"{method} /v1/credential-proxies/model{route} HTTP/1.1\r\nHost: guest-relay\r\n"
                   f"Authorization: Bearer {bearer}\r\n{entity}Connection: close\r\n\r\n{body or ''}")
        platform.guest(execution, ["sh", "-ec", 'umask 077; cat > "$1"', "save", folder + "/" + filename], message)

    write_request(ref, "first.req", "/fork-stream", token, upstream.model_request)
    # The shell survives its original outbound call. Each descendant supplies a new,
    # execution-bound request to that same shell through its own copy of the FIFO.
    job = """set -eu
cd "$1"
nonce=$(cat /proc/sys/kernel/random/uuid)
mkfifo continue
printf '%s %s\n' "$$" "$nonce" > identity
exec 3< first.req
rm first.req
"$2" --wait-model-stream <&3 3<&- > first.http 2>first.err &
reader=$!
exec 3<&-
echo "$reader" > reader
rc=0
wait "$reader" || rc=$?
echo "$rc" > first.done
IFS= read -r branch < continue
"$2" < next.req > next.http 2>next.err || { echo "$?" > next.rc; exit 1; }
rm -f next.req
printf '%s %s %s\n' "$$" "$nonce" "$branch" > result
"""
    platform.guest(ref, ["sh", "-ec", 'sh -c "$1" job "$2" "$3" </dev/null >/dev/null 2>&1 &',
                         "launch", job, folder, probe])
    assert upstream.started.wait(20), "the source model request did not arrive"
    started = time.monotonic()
    head, tail = (("data: " + value).encode() for value in (upstream.fork_head, upstream.fork_tail))
    wait_for(lambda: head in platform.guest(ref, ["cat", folder + "/first.http"]), timeout=12)
    identity = platform.guest(ref, ["cat", folder + "/identity"]).strip()
    platform.guest(ref, ["sh", "-ec", 'cd "$1"; test -d "/proc/$(cat reader)" && test ! -e first.done', "check", folder])
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id)
    status = worker(platform.worker, "GetLifecycleStatus", expected=ref, **scope)
    assert status["power"] == "RUNNING" and {"hold:credential_proxy", "keep_running"} <= set(status["blockers"]), status
    with closing(sqlite3.connect(platform.root / "catalog/catalog.sqlite")) as db:
        held, = db.execute("SELECT count(*) FROM lifecycle_protections WHERE principal=? AND incarnation_id=? AND state='active'",
                           (grant, ref["incarnation_id"])).fetchone()
        assert held == 1
    single = platform.args.case == "protected-stream-single"
    names = ("first",) if single else ("first", "second")
    endpoint, capability, body = ("forks", "state.independent_fork", dict(child_name="First answer")) if single else (
        "fork-batches", "state.fork_batch", dict(children=[dict(name="Answer " + name) for name in names]))
    accepted = platform.change(source + "/" + endpoint, "fork-waiting-for-model",
        {**body, "require_independent": True}, platform.wait_capability(source, capability))
    operation = accepted["operation_id"]
    with closing(sqlite3.connect((platform.root / "catalog/catalog.sqlite").as_uri() + "?mode=ro", uri=True)) as db:
        def finished():
            public = platform.app("operations/" + operation)
            if public["state"] == "succeeded":
                return public
            if public["state"] in ("failed", "cancelled", "recovery_required"):
                raise RuntimeError((operation, public["state"], public["effect"]))
            failed = db.execute(
                """SELECT c.ordinal, a.revision FROM managed_fork_children c
                   JOIN executions e ON e.last_operation_id=c.operation_id AND e.instance_id=c.instance_id
                   JOIN target_activations a ON a.operation_id=c.operation_id AND a.incarnation_id=e.incarnation_id
                   WHERE c.batch_id=? AND a.phase='failed_held' LIMIT 1""", (operation,)
            ).fetchone()
            if failed is not None:
                raise RuntimeError(("child activation failed_held", operation, *failed))
            return None

        try:
            result = wait_for(finished, timeout=175)
        except (RuntimeError, TimeoutError):
            public = platform.app("operations/" + operation)
            cut = db.execute("SELECT stage,source_released,credential_relay_hold,error FROM managed_fork_batches WHERE operation_id=?",
                             (operation,)).fetchone()
            children = db.execute(
                """SELECT c.ordinal,o.phase,e.phase,a.phase,a.decision,a.revision
                   FROM managed_fork_children c JOIN operations o ON o.operation_id=c.operation_id
                   LEFT JOIN executions e ON e.last_operation_id=c.operation_id AND e.instance_id=c.instance_id
                   LEFT JOIN target_activations a ON a.operation_id=c.operation_id AND a.incarnation_id=e.incarnation_id
                   WHERE c.batch_id=? ORDER BY c.ordinal""", (operation,)
            ).fetchall()
            print("STREAM_FORK_FAILURE " + json.dumps(dict(operation=operation, state=public["state"],
                effect=public["effect"], cut=cut, children=children)), flush=True)
            raise
    assert result["source_lock_released"] and result["source_disposition"] == "RUNNING", result
    assert len(result["children"]) == len(names) and all(c["state"] == "ready" and c["resource_independence"] == "verified"
                                                         for c in result["children"]), result
    assert upstream.requests == [("/fork-stream", True, True)], upstream.requests
    assert upstream.model_posts == [upstream.model_request.encode()]
    with closing(sqlite3.connect((platform.root / "catalog/catalog.sqlite").as_uri() + "?mode=ro", uri=True)) as db:
        cut = db.execute(
            """SELECT original,stage,source_released,credential_relay_hold FROM managed_fork_batches
               WHERE operation_id=?""", (result["operation_id"],)
        ).fetchone()
        protection = db.execute(
            "SELECT id,kind,state,reason FROM lifecycle_protections WHERE principal=? AND incarnation_id=?",
            (grant, ref["incarnation_id"]),
        ).fetchall()
        print("STREAM_CUT " + json.dumps(dict(operation=result["operation_id"], cut=cut, protection=protection)), flush=True)
    reader = platform.guest(ref, ["sh", "-ec",
        'cd "$1"; if test -f first.done; then cat first.done first.err; else test -d "/proc/$(cat reader)" && echo waiting; fi',
        "check", folder]).strip()
    elapsed = time.monotonic() - started
    print("STREAM_RELEASE " + json.dumps(dict(elapsed_seconds=round(elapsed, 3), source_reader=reader.decode())), flush=True)
    assert reader == b"waiting", "source stream reader closed before the provider released its final event"
    assert elapsed < 90, "the fixture exceeded its provider wait deadline"
    upstream.release.set()
    branches = [("source", machine, ref)]
    for name, child in zip(names, result["children"], strict=True):
        child_id = child["machine_id"]
        _, execution = platform.context(child_id)
        branches.append((name, child_id, execution))
        inherited = platform.app("machines/" + child_id + "/lifecycle")
        assert inherited["source"] == "parent" and all(inherited["desired"][k] == v for k, v in policy.items()), inherited
        assert not platform.app("machines/" + child_id + "/leases")["items"]
    assert len({r["incarnation_id"] for _, _, r in branches}) == len(branches)
    print("Model first event reached the live Guest; active hold, keep_running and lease", lease["lease_id"],
          "on source", ref["incarnation_id"], "during capture", result["result"]["capture_id"], flush=True)
    for name, machine_id, execution in branches:
        platform.guest(execution, ["test", "-d", "/proc/" + identity.split()[0].decode()])
        wait_for(lambda: platform.guest(execution, ["cat", folder + "/first.done"]), timeout=35, retry=(OSError, RuntimeError))
        old = platform.guest(execution, ["cat", folder + "/first.http"])
        print("STREAM_END " + json.dumps(dict(branch=name, head_count=old.count(head), tail_count=old.count(tail),
            bytes_received=len(old), upstream_original_posts=len(upstream.model_posts))), flush=True)
        assert old.count(head) == 1 and old.count(tail) == (1 if name == "source" else 0), name
        assert platform.guest(execution, ["sh", "-ec",
            'cd "$1" && test ! -e next.http && test ! -e result', "check", folder]) == b""
        next_grant, next_token = credential(platform, machine_id, "credential_proxy.use", "answer-" + name, proxy_id="model")
        wait_for((platform.relay_dir / (execution["incarnation_id"] + ".sock")).is_socket)
        def current_relay():
            status, payload, _ = wire(platform, execution, probe, "GET", "control", next_token)
            return status == 200 and json.loads(payload)["control_epoch"] == platform.epoch
        wait_for(current_relay, timeout=25, retry=(OSError, RuntimeError))
        path = "/answer/" + name
        write_request(execution, "next.req", path, next_token)
        platform.guest(execution, ["sh", "-ec", 'printf "%s\\n" "$1" > "$2/continue"', "reply", name, folder])
        def final_result():
            response = platform.guest(execution, ["sh", "-ec",
                'cd "$1"; if test -e next.rc; then cat next.rc next.err; elif test -e result; then cat result; fi', "result", folder])
            if response and not response.startswith(identity):
                raise AssertionError((name, response, upstream.requests))
            return response.strip()
        completed = wait_for(final_result, timeout=30, retry=(OSError, RuntimeError))
        assert completed == identity + b" " + name.encode(), completed
        reply = platform.guest(execution, ["cat", folder + "/next.http"])
        assert reply.startswith(b"HTTP/1.1 200") and ("data: " + upstream.answers[path]).encode() in reply
        assert all(value.encode() not in reply for route, value in upstream.answers.items() if route != path)
        with closing(sqlite3.connect(platform.root / "platform/platform.db")) as db:
            used = db.execute("SELECT source_machine_id,used_count FROM guest_grants WHERE id=?", (next_grant,)).fetchone()
            assert used == (machine_id, 1), used
        print("Original process", completed.decode(), "retained first event; original stream received final event",
              tail in old, "fresh answer from", path, flush=True)
    expected_paths = ["/fork-stream", *("/answer/" + name for name, _, _ in branches)]
    assert upstream.requests == [(p, True, True) for p in expected_paths], upstream.requests
    assert upstream.model_posts == [upstream.model_request.encode()]
    with closing(sqlite3.connect(platform.root / "catalog/catalog.sqlite")) as db:
        def settled():
            return db.execute("SELECT incarnation_id,state FROM lifecycle_protections WHERE principal=?",
                              (grant,)).fetchall() == [(ref["incarnation_id"], "released")]
        wait_for(settled, timeout=15)
    assert [entry["lease_id"] for entry in platform.app(source + "/leases")["items"] if entry["state"] == "active"] == [lease["lease_id"]]
    print(endpoint, "sent exactly one original model request and", len(branches),
          "independently authorized branch requests on", platform.bundle_id, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for field in ("bundle", "platformd", "system-image", "probe"):
        parser.add_argument("--" + field, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--case", choices=("delegate", "proxy", "protected-stream-fork", "protected-stream-single"), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--local-child", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if not args.local_child:
        subprocess.run(["unshare", "--mount", "--propagation", "private", "--net", "--", sys.executable,
                        __file__, *sys.argv[1:], "--local-child"], check=True)
        return
    fork_stream = args.case.startswith("protected-stream-")
    with tempfile.TemporaryDirectory(prefix="gcred-kvm-", dir=args.work_dir) as tmp, ExitStack() as stack:
        root = Path(tmp)
        root.chmod(0o755)
        if fork_stream:
            subprocess.run(["mount", "-t", "tmpfs", "-o", "size=7G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
            stack.callback(subprocess.run, ["umount", "-l", root], check=True)
            slice_name = f"smolvmstream{os.getpid()}_{os.urandom(4).hex()}.slice"
            subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                            "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                            slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"], check=True, stdout=subprocess.DEVNULL)
            stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=False)
            os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
        upstream = Upstream(root, stack, fork_stream=fork_stream) if args.case != "delegate" else None
        platform = Platform(args, root, stack, credential_proxies=[upstream.policy] if upstream else (), use_installation=fork_stream)
        try:
            platform.start()
            if upstream:
                settings = platform.app("system/settings")
                platform.app("system/settings", "PATCH", {"default_lifecycle_policy": {
                    "auto_standby": False, "auto_hibernate": False, "min_running_after_wake_seconds": 0,
                }}, {"If-Control-Epoch": platform.epoch, "Idempotency-Key": "fixture-lifecycle-defaults",
                     "If-Match": f'"{settings["revision"]}"'})
            source = platform.change("machines", "source", dict(name="Guest authority source", template_id="t_system",
                boot_mode="system", runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=2,memory_mib=768)))
            platform.complete(source)
            machine = source["machine_id"]
            _, ref = platform.context(machine)
            wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
            probe = platform.place_probe(ref, args.probe)
            if fork_stream:
                protected_stream_fork(platform, upstream, machine, ref, probe)
            elif upstream:
                proxy(platform, upstream, machine, ref, probe)
            else:
                delegated(platform, machine, ref, probe)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2400:], file=sys.stderr)
            for log in (root / "state/managed").glob("vmm-*.log"):
                lines = log.read_text(errors="replace").splitlines()
                gate = [line for line in lines if "gate" in line.lower() or "activation" in line.lower()]
                if gate:
                    print(log.name, *gate[-12:], sep="\n", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
