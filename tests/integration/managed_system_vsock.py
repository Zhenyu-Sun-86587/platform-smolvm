#!/usr/bin/env python3
"""Exercise managed System vsock on Linux/KVM through installed platform and Worker binaries."""

import argparse
import array
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, suppress
from datetime import datetime, timedelta, timezone
import grp
import hashlib
from http.client import HTTPConnection, HTTPResponse, HTTPSConnection
import io
import json
import os
from pathlib import Path
import select
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time


EXT = "/api/v1/runtime-ext/v1"
INSTALLED_CASES = {"fork-lost-reply", "task-fork-cancel-retry", "grant-rollback", "saved-wake-exact", "polish-files", "polish-capture", "polish-lease-fork", "polish-metrics", "polish-history", "polish-old-agent", "polish-metrics-epoch"}


class UnixHTTPConnection(HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=60)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.path))


def request(client, method, path, body=None, headers=None, status=200):
    try:
        client.request(method, path, None if body is None else json.dumps(body), {
            "Content-Type": "application/json", **(headers or {}),
        })
        response = client.getresponse()
        payload = response.read()
        if response.status != status:
            if path.endswith("/credential"):
                payload = b"<guest credential response>"
            raise RuntimeError((path, response.status, payload))
        return json.loads(payload), response.getheader("Set-Cookie")
    finally:
        client.close()


def worker(endpoint, method, payload=None, fd=None, **scope):
    packet = dict(protocol="WorkerResource/1", request_id=method, method=method,
                  payload=payload or {}, **scope)
    rights = []
    if fd is not None:
        packet["fd_table"] = [
            dict(index=0, role="block_import", resource_id=payload["resource_id"])
        ]
        rights = [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", [fd]))]
    with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as connection:
        connection.settimeout(150)
        connection.connect(str(endpoint))
        connection.sendmsg([json.dumps(packet).encode()], rights)
        response = json.loads(connection.recv(65536))
    if "error" in response:
        raise RuntimeError(response["error"])
    return response["result"]


def wait_for(probe, timeout=90, retry=(OSError,)):
    end = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < end:
        try:
            if value := probe():
                return value
        except retry as error:
            last_error = error
        time.sleep(0.2)
    raise TimeoutError(f"fixture command exceeded its deadline: {last_error}")


def digest(path):
    with path.open("rb") as source:
        return "sha256:" + hashlib.file_digest(source, "sha256").hexdigest()


class Platform:
    def __init__(self, args, root, stack, networks=(), credential_proxies=(), state_tmpfs=False, use_installation=False,
                 *, tls=None, gateway=None, managed_ssh=False):
        self.args = args
        self.root = root
        self.stack = stack
        self.worker = root / "ipc/worker.sock"
        self.serve = root / "api/serve.sock"
        self.relay_dir = root / "guest-relay"
        self.cookie = ""
        self.csrf = ""
        self.epoch = ""
        self.networks = networks
        self.credential_proxies = credential_proxies
        self.state_tmpfs = state_tmpfs
        self.use_installation = use_installation
        self.tls = tls
        self.gateway = gateway
        self.managed_ssh = managed_ssh

    def api_connection(self, timeout=15):
        if self.tls:
            return HTTPSConnection("127.0.0.1", self.port, context=self.tls[2], timeout=timeout)
        return HTTPConnection("127.0.0.1", self.port, timeout=timeout)

    def start(self, *, owner_password=None):
        root = self.root
        for name in ("var-lib", "ipc", "runtime"):
            (root / name).mkdir(mode=0o755)
        if self.state_tmpfs:
            state = root / "platform"
            state.mkdir(mode=0o700)
            subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G,mode=0700,nosuid,nodev", "tmpfs", state], check=True)
            self.stack.callback(subprocess.run, ["umount", state], check=True)
        subprocess.run(["mount", "--bind", root / "var-lib", "/var/lib"], check=True)
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
        runtime = Path("/run/smolvm-custom")
        runtime.mkdir(exist_ok=True)
        subprocess.run(["mount", "--bind", root / "runtime", runtime], check=True)
        self.bundle = root / "bundle"
        if self.use_installation:
            self.bundle.mkdir()
            subprocess.run(["mount", "--rbind", self.args.bundle, self.bundle], check=True)
            self.stack.callback(subprocess.run, ["umount", "--lazy", self.bundle], check=True)
            subprocess.run(["mount", "-o", "remount,bind,ro", self.bundle], check=True)
        else:
            shutil.copytree(self.args.bundle, self.bundle, symlinks=True)
        self.env = {
            **os.environ,
            "LD_LIBRARY_PATH": str(self.bundle / "lib"),
            "SMOLVM_LIB_DIR": str(self.bundle / "lib"),
            "SMOLVM_GUEST_RELAY_DIR": str(self.relay_dir),
        }
        password = owner_password or os.urandom(24).hex()
        secret = root / "owner-password"
        secret.touch(mode=0o600)
        secret.write_text(password + "\n")
        self.env["SANDBOX_OWNER_PASSWORD_FILE"] = str(secret)
        catalog = root / "catalog"
        catalog.mkdir(mode=0o700, exist_ok=True)
        # Router nsfs pins outlive a Worker process, including a fixture's SIGKILL.
        subprocess.run(["mount", "--bind", catalog, catalog], check=True)
        self.stack.callback(subprocess.run, ["umount", "--lazy", catalog], check=True)
        self.broker = self.spawn([self.bundle / "smolvm-resourced", "serve", "--socket", self.worker,
                                  "--state-dir", catalog, "--socket-gid", str(grp.getgrnam("kvm").gr_gid)],
                                 "worker.log")
        wait_for(lambda: worker(self.worker, "Health")["state"] == "ready")

        self.spawn([self.bundle / "smolvm-bin", "--state-dir", root / "state",
                    "serve", "start", "--listen", f"unix://{self.serve}",
                    "--worker-resource-socket", self.worker, "--guest-rollout-port", "37451",
                    "--shutdown-grace", "0"], "serve.log")
        handshake = wait_for(lambda: self.native("/capabilities"), retry=(OSError, RuntimeError))
        self.bundle_id = handshake["bundle_digest"]
        self.serve_id = handshake["runtime_instance_id"]
        self.native("/machines/import-template/binding")
        self.stack.callback(self.kill_vms)
        image_hash = digest(self.args.system_image)
        import_scope = dict(route_id="deep", bundle_digest=self.bundle_id,
                            operation_id="import-system-fixture", request_digest=image_hash,
                            expected=dict(instance_id="template", incarnation_id=None, execution_epoch=0))
        worker(self.worker, "BeginOperation", {"kind": "catalog"}, **import_scope)
        with self.args.system_image.open("rb") as image:
            worker(self.worker, "ImportImmutableBlock", {
                "resource_id": "fixture-system-root",
                "capacity_bytes": self.args.system_image.stat().st_size,
                "content_sha256": image_hash,
            }, fd=image.fileno(), **import_scope)
        worker(self.worker, "FinishOperation", {"abort": False, "expected_revision": 1}, **import_scope)
        templates = root / "templates.json"
        templates.write_text(json.dumps([dict(
            template_id="t_system", name="Debian", version=1, image_digest=image_hash,
            guest_release="system-debian-v1",
            spec=dict(boot_mode="system", root_generation_id="fixture-system-root",
                      **({"managed_ssh": True} if self.managed_ssh else {})),
        )]))
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        scheme = "https" if self.tls else "http"
        self.origin = f"{scheme}://127.0.0.1:{self.port}"
        api = dict(listen=f"127.0.0.1:{self.port}", public_origin=self.origin)
        if self.tls:
            api.update(tls_cert_file=str(self.tls[0]), tls_key_file=str(self.tls[1]))
        config = root / "platform.json"
        config.write_text(json.dumps(dict(
            version=1, state_dir=str(root / "platform"), templates_file=str(templates),
            api=api,
            **({"gateway": self.gateway} if self.gateway else {}),
            **({} if self.state_tmpfs else {"storage": dict(minimum_free_bytes=2 << 30, minimum_free_percent=3)}),
            **({"credential_proxies": self.credential_proxies} if self.credential_proxies else {}),
            runtime=dict(socket=str(root / "ipc/official.sock"), status_socket="",
                         data_dir=str(root / "official"), custom=[dict(
                             instance_id=self.serve_id, bundle_digest=self.bundle_id,
                             state_reader_id=handshake["state_reader_id"], socket=str(self.serve),
                             manifest=str(self.bundle / "runtime-bundle.json"),
                             worker_socket=str(self.worker), guest_relay_dir=str(self.relay_dir),
                             private_networks=self.networks,
                         )]),
        )))
        self.platform_command = [self.args.platformd, "--config", config]
        self.platform = self.spawn(self.platform_command, "platform.log")
        self.login_owner(password)
        secret.unlink()

    def login_owner(self, password):
        session, cookie = wait_for(lambda: request(
            self.api_connection(), "POST", "/v1/auth/login",
            dict(username="owner", password=password), {"Origin": self.origin},
        ), retry=(OSError,))
        self.cookie = cookie.split(";", 1)[0]
        self.csrf = session["csrf_token"]
        self.epoch = self.app("control")["control_epoch"]

    def spawn(self, command, log):
        stream = self.stack.enter_context((self.root / log).open("ab"))
        process = subprocess.Popen(command, env=self.env, stdin=subprocess.DEVNULL,
                                   stdout=stream, stderr=stream)

        def stop():
            if process.poll() is None:
                process.kill()
            process.wait()

        self.stack.callback(stop)
        return process

    def kill_vms(self):
        for file in (self.root / "catalog").glob("*.sqlite"):
            with suppress(OSError, sqlite3.Error), sqlite3.connect(file) as db:
                for pid, start in db.execute("SELECT pid,pid_start_time FROM executions"):
                    with suppress(OSError):
                        fd = os.pidfd_open(pid)
                        try:
                            actual = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
                            if actual == start:
                                signal.pidfd_send_signal(fd, signal.SIGKILL)
                        finally:
                            os.close(fd)

    def app(self, path, method="GET", body=None, headers=None, status=200, *, timeout=15):
        return request(self.api_connection(timeout), method, "/v1/" + path,
                       body, {"Origin": self.origin, "Cookie": self.cookie,
                              "X-CSRF-Token": self.csrf, **(headers or {})}, status)[0]

    def native(self, path, body=None):
        return request(UnixHTTPConnection(self.serve), "GET" if body is None else "POST",
                       EXT + path, body)[0]

    def change(self, path, key, body, machine=None, method="POST", status=202):
        headers = {"If-Control-Epoch": self.epoch, "Idempotency-Key": key}
        if machine is not None:
            headers["If-Match"] = f'"{machine["revision"]}"'
            body = {**body, "expected_generation": machine["binding_generation"]}
        return self.app(path, method, body, headers, status)

    def complete(self, accepted):
        def finished():
            result = self.app("operations/" + accepted["operation_id"])
            if result["state"] in ("failed", "cancelled", "recovery_required"):
                raise RuntimeError(result)
            return result if result["state"] == "succeeded" else None

        return wait_for(finished, timeout=175)

    def restart(self):
        self.platform.kill()
        self.platform.wait()
        self.platform = self.spawn(self.platform_command, "platform.log")
        wait_for(lambda: self.app("control"))

    def context(self, machine_id):
        view = self.app("machines/" + machine_id)
        ref = self.native(f"/machines/{view['runtime_name']}/execution")
        return view, ref

    def wait_capability(self, machine_path, capability, timeout=35):
        def supported():
            view = self.app(machine_path)
            return view if view["capabilities"][capability]["supported"] else None
        return wait_for(supported, timeout=timeout)

    def guest(self, ref, command, stdin=None, expected=0):
        command = dict(command=command, timeout_seconds=45)
        if stdin is not None:
            command["stdin"] = stdin
        result = self.native(f"/instances/{ref['instance_id']}/guest/exec",
                             dict(expected=ref, request=command))["result"]
        if result["exit_code"] != expected:
            raise RuntimeError(base64.b64decode(result["stderr"]))
        return base64.b64decode(result["stdout"])

    def agent_socket(self, ref):
        return Path("/run/smolvm-custom/managed") / self.serve_id / ref["incarnation_id"] / "agent.sock"

    def place_probe(self, ref, binary):
        path = "/tmp/managed-vsock-probe"
        self.guest(ref, ["/bin/sh", "-c", 'umask 077; : > "$1"', "setup", path])
        with binary.open("rb") as source:
            while chunk := source.read(32768):
                self.guest(ref, ["/bin/sh", "-c", 'base64 -d >> "$1"', "append", path],
                           base64.b64encode(chunk).decode())
        self.guest(ref, ["chmod", "700", path])
        return path


def source_peer(platform, ref):
    connection = platform.stack.enter_context(socket.socket(socket.AF_UNIX))
    connection.settimeout(8)
    connection.connect(str(platform.agent_socket(ref)))
    # Wait for the VMM to accept and establish this stream before initiating capture.
    time.sleep(0.3)
    return connection


def assert_fenced(peer):
    try:
        assert peer.recv(1) == b""
    except ConnectionResetError:
        pass


def fork_after_lost_reply(platform, source, ref):
    view = platform.wait_capability(source, "state.fork_batch")
    key = "original-fork-response-lost"
    body = dict(children=[dict(name="Only child of unacknowledged batch")], require_independent=True)
    marker = os.urandom(20).hex()
    platform.guest(ref, ["/bin/sh", "-ec", 'printf %s "$1" > /run/original-fork-marker', "write", marker])
    path = source + "/fork-batches"
    client = HTTPConnection("127.0.0.1", platform.port, timeout=20)
    try:
        client.request("POST", "/v1/" + path, json.dumps({**body, "expected_generation": view["binding_generation"]}),
                       {"Content-Type": "application/json", "Origin": platform.origin, "Cookie": platform.cookie,
                        "X-CSRF-Token": platform.csrf, "If-Control-Epoch": platform.epoch, "Idempotency-Key": key,
                        "If-Match": f'"{view["revision"]}"'})
        # Bytes are waiting in the kernel. Discard the entire response without reading its operation or children.
        assert select.select([client.sock], [], [], 20)[0]
        assert client.sock.recv(1, socket.MSG_PEEK) == b"H"
    finally:
        client.close()
    database = platform.root / "platform/platform.db"
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as db:
        first, = db.execute("SELECT operation_id,request_hash FROM idempotency WHERE key=?", (key,)).fetchall()
    platform.restart()
    repeated = platform.change(path, key, body, view)
    assert repeated["operation_id"] == first[0]
    result = platform.complete(repeated)
    child, = result["children"]
    assert child["state"] == "ready" and child["machine_id"] == repeated["children"][0]["machine_id"]
    _, child_ref = platform.context(child["machine_id"])
    assert platform.guest(child_ref, ["cat", "/run/original-fork-marker"]).decode() == marker
    capture = result["result"]["capture_id"]
    with sqlite3.connect((platform.root / "catalog/catalog.sqlite").as_uri() + "?mode=ro", uri=True) as catalog:
        assert catalog.execute("""SELECT operation_id,capture_id FROM managed_fork_batches WHERE source_incarnation=?""",
                               (ref["incarnation_id"],)).fetchall() == [(first[0], capture)]
        assert catalog.execute("SELECT instance_id,attempt FROM managed_fork_children WHERE batch_id=?",
                               (first[0],)).fetchall() == [(child_ref["instance_id"], 0)]
    assert platform.change(path, key, body, view)["operation_id"] == first[0]
    conflict = platform.change(path, key, {**body, "children": [dict(name="Changed child")]}, view, status=409)
    assert conflict["error"]["code"] == "IDEMPOTENCY_CONFLICT", conflict
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as db:
        assert db.execute("SELECT operation_id,request_hash FROM idempotency WHERE key=?", (key,)).fetchall() == [first]
    print("Original response discarded; platform restarted; retries returned one batch", first[0],
          "one capture", capture, "one original child", child["machine_id"], "with RAM marker", marker,
          "bundle", platform.bundle_id, flush=True)


def inherited_agent(platform, ref, probe, seen_cids):
    marker = wait_for(lambda: platform.guest(ref, ["/bin/cat", "/var/tmp/platform-cut-marker"]),
                      timeout=25, retry=(RuntimeError, OSError))
    assert marker == b"held-source\n", marker
    cid = int(platform.guest(ref, [probe, "--cid"]))
    assert cid >= 3 and cid not in seen_cids, (cid, seen_cids)
    seen_cids.add(cid)
    assert platform.agent_socket(ref).is_socket()
    return cid


def fork_case(platform, source, ref, probe, seen_cids):
    peer = source_peer(platform, ref)
    view = platform.app(source)
    body = dict(children=[dict(name="Debian child one"), dict(name="Debian child two")],
                require_independent=True)
    accepted = platform.change(source + "/fork-batches", "fork", body, view)
    completed = platform.complete(accepted)
    assert completed["source_lock_released"] and completed["source_disposition"] == "RUNNING"
    assert_fenced(peer)
    assert int(platform.guest(ref, [probe, "--cid"])) in seen_cids
    assert len(completed["children"]) == 2
    for child in completed["children"]:
        assert child["state"] == "ready" and child["resource_independence"] == "verified", child
        view, child_ref = platform.context(child["machine_id"])
        assert view["power_state"] == "running" and child_ref["incarnation_id"] != ref["incarnation_id"]
        inherited_agent(platform, child_ref, probe, seen_cids)
    print("System fork closed the old source stream and rebound two independent Agent sockets and CIDs.")
    return completed["children"]


def agent_unavailable(platform, execution):
    try:
        platform.native(f"/instances/{execution['instance_id']}/guest/ping", dict(expected=execution, request={}))
    except RuntimeError as error:
        _, status, body = error.args[0]
        detail = json.loads(body)
        assert status == 503 and detail.get("code") == "E_GUEST_MANAGEMENT_UNAVAILABLE", (status, detail)
        return True
    return False


def hold_fork_targets(catalog, op_id, handles, include_failed):
    actions = {1: signal.SIGSTOP}
    if include_failed:
        actions[2] = signal.SIGKILL
    until = time.monotonic() + 45
    pending = None
    while actions and time.monotonic() < until:
        targets = catalog.execute("""SELECT c.ordinal,e.pid,e.pid_start_time FROM managed_fork_children c
            JOIN executions e ON e.last_operation_id=c.operation_id
            WHERE c.batch_id=? AND c.ordinal IN (1,2) AND e.phase!='exited' AND NOT EXISTS (
            SELECT 1 FROM target_activations a WHERE a.operation_id=c.operation_id AND a.decision='commit')""", (op_id,)).fetchall()
        for ordinal, pid, start in targets:
            if ordinal not in actions:
                continue
            subject = handles.enter_context(os.fdopen(os.pidfd_open(pid), "rb", buffering=0))
            assert int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]) == start
            signal.pidfd_send_signal(subject.fileno(), actions.pop(ordinal))
            if ordinal == 1:
                pending = subject
        if actions:
            time.sleep(0.005)
    if actions:
        raise TimeoutError(f"fork targets not observed before commit: {sorted(actions)}")
    return pending


def cancel_partial_fork(platform, source, ref, retry_after_parent_delete=False):
    marker = os.urandom(16).hex()
    if not retry_after_parent_delete:
        platform.guest(ref, ["/usr/bin/tee", "/var/tmp/fork-cancel-marker"], marker)
    view = platform.wait_capability(source, "state.fork_batch")
    path = source + "/fork-batches"
    body = dict(children=[dict(name="Keep committed child"), dict(name="Cancel pending child")], require_independent=True)
    if retry_after_parent_delete:
        body["children"].append(dict(name="Retry failed child"))
    accepted = platform.change(path, "fork-to-cancel", body, view)
    op_id = accepted["operation_id"]
    operation = "operations/" + op_id
    with sqlite3.connect(f"file:{platform.root}/catalog/catalog.sqlite?mode=ro", uri=True) as db:
        with ExitStack() as handles:
            pending = hold_fork_targets(db, op_id, handles, retry_after_parent_delete)
            try:
                def committed_child():
                    current = platform.app(operation)
                    if retry_after_parent_delete and current["children"][2].get("error_code") != "NATIVE_CHILD_ABORTED":
                        return None
                    return current if current["children"][0]["state"] == "ready" else None
                partial = wait_for(committed_child, timeout=40)
                assert partial["state"] == "observing" and partial["effect"] == "partial", partial
                capture = partial["result"]["capture_id"]
                first = partial["children"][0]
                assert first["resource_independence"] == "verified" and partial["children"][1]["state"] == "pending"
                requested = platform.change(operation + "/cancel", "cancel-pending-fork", {}, status=200)
                assert requested["stage"] == "fork_cancel_requested" and requested["effect"] == "partial", requested
                platform.restart()
                assert platform.change(operation + "/cancel", "cancel-pending-fork", {}, status=200) == requested
                wait_for(lambda: db.execute("SELECT cancel_requested FROM managed_fork_batches WHERE operation_id=?",
                                            (op_id,)).fetchone()[0], timeout=40)
            finally:
                with suppress(ProcessLookupError):
                    signal.pidfd_send_signal(pending.fileno(), signal.SIGCONT)
            final = platform.complete(accepted)
            assert select.select([pending], [], [], 15)[0], "cancelled target VMM still alive"
        assert final["effect"] == "partial" and final["result"]["capture_id"] == capture, final
        assert final["children"][0] == first and final["children"][1]["error_code"] == "FORK_CHILD_CANCELLED", final
        assert final["children"][1]["machine_id"] == accepted["children"][1]["machine_id"]
        assert platform.change(path, "fork-to-cancel", body, view)["operation_id"] == op_id
        assert db.execute("SELECT count(*) FROM managed_fork_batches WHERE capture_id=?", (capture,)).fetchone() == (1,)
    if retry_after_parent_delete:
        assert final["children"][2]["error_code"] == "NATIVE_CHILD_ABORTED", final
        assert agent_unavailable(platform, ref)
        retire_task_parent(platform, source, ref)
        headers = {"If-Control-Epoch": platform.epoch, "Idempotency-Key": "retry-failed-fork", "If-Match": f'"{final["revision"]}"'}
        retry = platform.app(operation + "/retry", "POST", {}, headers, 202)
        assert platform.change(operation + "/cancel", "cancel-pending-fork", {}, status=200) == requested
        recovered = platform.complete(retry)
        assert recovered["result"]["capture_id"] == capture and recovered["effect"] == "partial", recovered
        assert recovered["children"][:2] == final["children"][:2], recovered
        third = recovered["children"][2]
        assert third["state"] == "ready" and third["attempt"] == 1 and third["machine_id"] == final["children"][2]["machine_id"], third
        assert platform.app(operation + "/retry", "POST", {}, headers, 202) == retry
        print(f"In cut {capture}, public cancellation preserved one ready and one already failed target. "
              f"After public parent deletion, the sole failed target {third['machine_id']} became ready on attempt 1; "
              "the original cancelled target stayed fenced.", flush=True)
        return third["machine_id"]
    _, child_ref = platform.context(first["machine_id"])
    assert platform.guest(child_ref, ["/bin/cat", "/var/tmp/fork-cancel-marker"]).decode() == marker
    assert platform.guest(ref, ["/bin/cat", "/var/tmp/fork-cancel-marker"]).decode() == marker
    print(f"Public in-flight cancel retained committed child {first['machine_id']} and cut {capture}. "
          f"The pending VMM exited, both keys replayed after platform restart, and source and child returned the captured marker.", flush=True)


def vsock_probe_binary(args, root):
    binary = args.probe
    if binary is None:
        binary = root / "vsock-probe"
        source_file = Path(__file__).resolve().parents[1] / "fixtures/vsock-probe.c"
        subprocess.run([args.cc, "-static", "-Os", "-s", "-o", binary, source_file], check=True)
    return binary


def stop_agent_until_fork(platform, ref):
    probe = platform.place_probe(ref, vsock_probe_binary(platform.args, platform.root))
    source_cid = int(platform.guest(ref, [probe, "--cid"]))
    assert source_cid >= 3
    # A fork resumes the same guest process on a distinct vsock CID. The
    # parent remains offline through retirement, independent of host speed.
    resume = ('while :; do child=$("$1" --cid) && [ "$child" -ge 3 ] && [ "$child" != "$2" ] '
              '&& exec systemctl start smolvm-agent.service; sleep 1; done')
    platform.guest(ref, ["systemd-run", "--unit=fixture-agent-child-resume", "/bin/sh", "-c",
                         resume, "resume-after-fork", probe, str(source_cid)])
    platform.guest(ref, ["systemd-run", "--unit=fixture-agent-stop", "--on-active=1s",
                         "--timer-property=AccuracySec=100ms", "systemctl", "stop", "smolvm-agent.service"])
    wait_for(lambda: agent_unavailable(platform, ref), timeout=15)


def stop_agent_for_capture(platform, ref):
    for action, seconds in (("start", 60), ("stop", 1)):
        platform.guest(ref, ["/usr/bin/systemd-run", "--unit=fixture-agent-" + action, f"--on-active={seconds}s",
                             "--timer-property=AccuracySec=100ms", "/usr/bin/systemctl", action, "smolvm-agent.service"])
    wait_for(lambda: agent_unavailable(platform, ref), timeout=15)


def gate_without_agent(platform, source, ref, probe, seen_cids):
    script = "/var/tmp/gate-counter.sh"
    platform.guest(ref, ["/usr/bin/tee", script], r"""set -eu
path=/var/tmp/gate-counter-$1
i=0
while :; do
    printf '%s %s\n' "$$" "$i" > "$path.next"
    mv "$path.next" "$path"
    i=$((i+1))
    sleep 0.1
done
""")
    for label, prefix in (("system", []), ("nested", ["/usr/bin/unshare", "--pid", "--fork", "--mount-proc"])):
        platform.guest(ref, ["/usr/bin/systemd-run", "--unit=fixture-counter-" + label, *prefix,
                             "/bin/sh", script, label])

    def counts(execution):
        files = ["/var/tmp/gate-counter-system", "/var/tmp/gate-counter-nested"]
        rows = platform.guest(execution, ["/bin/cat", *files]).splitlines()
        return [tuple(map(int, row.split())) for row in rows]

    def advanced(execution, previous):
        sample = counts(execution)
        return (len(sample) == 2 and all(count > old for (_, count), old in zip(sample, previous))
                and sample)

    before = wait_for(lambda: advanced(ref, [2, 2]), retry=(RuntimeError, OSError))
    assert before[0][0] > 1 and before[1][0] == 1, before
    stop_agent_for_capture(platform, ref)
    op = platform.complete(platform.change(source + "/fork-batches", "fork-with-agent-stopped", {
        "children": [{"name": "Agent stopped child"}], "require_independent": True,
    }, platform.app(source)))
    assert op["source_lock_released"] and op["source_disposition"] == "RUNNING", op
    (child,) = op["children"]
    view, target = platform.context(child["machine_id"])
    assert view["power_state"] == "running" and target["incarnation_id"] != ref["incarnation_id"], view
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    assert agent_unavailable(platform, ref) and agent_unavailable(platform, target), "Agent restarted before the fork committed"
    print("The ordinary Guest Agent was unavailable before capture and on both VMs after the independent fork committed.", flush=True)
    for label, execution in (("source", ref), ("child", target)):
        after = wait_for(lambda: advanced(execution, [n for _, n in before]), retry=(RuntimeError, OSError))
        assert [pid for pid, _ in after] == [pid for pid, _ in before], (before, after)
        print(f"{label}: same System and nested PID counters advanced from {before} to {after}.", flush=True)
    inherited_agent(platform, target, probe, seen_cids)


def task_wake_case(platform, source, ref):
    before = wait_for(lambda: (v if (v := platform.app(source)).get("task_metadata_status") == "complete" else None))
    platform.complete(platform.change(source + "/hibernate", "before-task", {}, before))
    for state in ("hibernated", "standby"):
        saved = platform.app(source)
        assert saved["power_state"] == state, saved
        body = {"argv": ["/bin/sh", "-c", "echo ready; exec sleep 240"], "task_key": "task-from-" + state}
        accepted = platform.change(source + "/tasks", "start-from-" + state, body, saved)
        started = platform.complete(accepted)
        assert platform.change(source + "/tasks", "start-from-" + state, body, saved)["operation_id"] == accepted["operation_id"]
        machine, live = platform.context(saved["machine_id"])
        assert machine["power_state"] == "running" and live["execution_epoch"] == ref["execution_epoch"] + 1, live
        if state == "hibernated":
            restored = live
        else:
            assert live == restored, (live, restored)
        task = "tasks/" + started["result"]["task_id"]
        wait_for(lambda: b"ready" in base64.b64decode(platform.app(task + "/output")["data_base64"]))
        cancelled = platform.complete(platform.app(task + "/cancel", "POST", {"grace_seconds": 1}, {
            "If-Control-Epoch": platform.epoch, "Idempotency-Key": "cancel-" + state,
            "If-Match": f'"{machine["revision"]}"',
        }, 202))
        assert cancelled["result"]["state"] == "cancelled" and platform.app(task)["state"] == "cancelled", cancelled
        if state == "hibernated":
            platform.complete(platform.change(source + "/standby", "standby-for-task", {}, platform.app(source)))
        print(f"A public task woke {state}, returned live output and was cancelled on confirmed epoch {live['execution_epoch']}.", flush=True)


def native_task_case(platform, source, ref, case, volume=None):
    initial = wait_for(lambda: (v if (v := platform.app(source)).get("task_metadata_status") == "complete" else None))
    source_id = initial["machine_id"]
    command = """set -eu
mkfifo /run/task-reply
nonce=$(cat /proc/sys/kernel/random/uuid)
printf '%s %s\\n' "$$" "$nonce" >/run/task-id
read -r reply </run/task-reply
printf '%s %s %s\\n' "$$" "$nonce" "$reply"
"""
    body = dict(argv=["/bin/sh", "-ec", command], task_key="original-task")
    started = platform.complete(platform.change(source + "/tasks", "original-task", body, initial))
    original_id = started["result"]["task_id"]
    original = platform.app("tasks/" + original_id)
    marker = wait_for(lambda: platform.guest(ref, ["/bin/cat", "/run/task-id"]).strip(), retry=(OSError, RuntimeError))
    assert original["machine_id"] == source_id and original["state"] == "running", original
    assert original["runner"]["pid"] > 1 and not original.get("origin_task_id"), original
    if case == "task-restore":
        saved = platform.complete(platform.change(source + "/snapshots", "task-checkpoint", {"capture_mode": "vm"}, platform.app(source)))
        target = platform.complete(platform.change("restores", "restore-task", dict(snapshot_id=saved["result"]["snapshot_id"],
            name="Restored task", volume_restore_policy="clone_conflicts")))
        target_id = target["machine_id"]
    elif case == "task-fork-cancel-retry":
        platform.wait_capability(source, "state.fork_batch")
        stop_agent_until_fork(platform, ref)
        target_id = cancel_partial_fork(platform, source, ref, retry_after_parent_delete=True)
    else:
        wait_for(lambda: platform.app(source)["capabilities"]["state.fork_batch"]["status"] == "supported")
        stop_agent_for_capture(platform, ref)
        accepted_view = platform.wait_capability(source, "state.fork_batch")
        assert agent_unavailable(platform, ref)
        path = source + "/fork-batches"
        body = dict(children=[dict(name="Live child"), dict(name="Over budget")], require_independent=True, admission="best_effort")
        accepted = platform.change(path, "same-task-cut", body, accepted_view)
        finished = platform.complete(accepted)
        retained, refused = finished["children"]
        assert finished["effect"] == "partial" and finished["source_lock_released"], finished
        assert retained["state"] == "ready" and refused["error_code"] == "RESOURCE_LIMIT", finished
        assert platform.change(path, "same-task-cut", body, accepted_view)["operation_id"] == accepted["operation_id"]
        denied = platform.change("operations/" + finished["operation_id"] + "/cancel", "late-cancel", {}, status=409)
        assert denied["error"]["code"] == "OPERATION_IN_PROGRESS", denied
        after = platform.app("operations/" + finished["operation_id"])
        assert after["result"]["capture_id"] == finished["result"]["capture_id"] and after["children"] == finished["children"]
        target_id = retained["machine_id"]
        _, child_ref = platform.context(target_id)
        assert agent_unavailable(platform, child_ref)
        assert platform.app("machines/" + target_id)["task_metadata_status"] == "unavailable"
        assert platform.app("machines/" + target_id + "/tasks")["items"] == []
    if case == "task-adoption-delete":
        retire_task_parent(platform, source, ref)
    elif case != "task-fork-cancel-retry":
        vm = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]
        with os.fdopen(os.pidfd_open(vm["pid"]), "rb", buffering=0) as original_vmm:
            assert int(Path(f"/proc/{vm['pid']}/stat").read_text().rsplit(")", 1)[1].split()[19]) == vm["pid_start_time"]
            signal.pidfd_send_signal(original_vmm.fileno(), signal.SIGKILL)
            assert select.select([original_vmm], [], [], 10)[0]
        platform.restart()
    machine = "machines/" + target_id
    _, child_ref = platform.context(target_id)
    def adopted():
        return next((t for t in platform.app(machine + "/tasks")["items"] if t.get("origin_task_id") == original_id), None)
    child = wait_for(adopted)
    assert child["machine_id"] == target_id and child["origin_machine_id"] == source_id and child["id"] != original_id, child
    assert child["task_namespace"] != original["task_namespace"] and child["run_id"] == original["run_id"], child
    assert platform.guest(child_ref, ["/bin/cat", "/run/task-id"]).strip() == marker
    platform.guest(child_ref, ["/bin/sh", "-c", "echo child >/run/task-reply"])
    task = "tasks/" + child["id"]
    ended = wait_for(lambda: (v if (v := platform.app(task))["state"] == "succeeded" else None))
    data = base64.b64decode(platform.app(task + "/output")["data_base64"]).strip()
    assert ended["exit_code"] == 0 and data == marker + b" child", (ended, data)
    print(f"{case}: after source VMM exit and platform restart, public task {child['id']} retained run {child['run_id']} "
          f"and returned the original process identity {data.decode()}.", flush=True)
    if volume is not None:
        verify_volume_after_retirement(platform, volume, child_ref)


def retire_task_parent(platform, source, ref):
    parent = platform.app(source)
    assert parent["power_state"] == "running", parent
    denied = platform.change(source, "reject-live-delete", {}, parent, method="DELETE", status=409)
    assert denied["error"]["code"] == "STATE_CONFLICT", denied
    stopped = platform.complete(platform.change(source + "/stop", "fence-delete-source", {}, parent))
    assert stopped["source_disposition"] == "EXITED", stopped
    parent = platform.app(source)
    accepted = platform.change(source, "retire-original-source", {}, parent, method="DELETE")
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id, operation_id=accepted["operation_id"])
    wait_for(lambda: worker(platform.worker, "QueryOperation", **scope), retry=(OSError, RuntimeError))
    platform.restart()
    retired = platform.complete(accepted)
    assert retired["source_disposition"] == "EXITED" and retired["result"]["execution_epoch"] == ref["execution_epoch"], retired
    assert platform.change(source, "retire-original-source", {}, parent, method="DELETE") == retired
    tombstone = platform.app(source)
    assert tombstone["desired_power"] == "deleted" and tombstone["deleted_at"] and not tombstone.get("binding"), tombstone
    empty = {**ref, "incarnation_id": None}
    try:
        worker(platform.worker, "RequestWake", dict(protection_id="access-retired-source", principal="fixture-owner",
            trigger="service_access", expected_binding_revision=2 * ref["execution_epoch"], authorization_revision=1,
            endpoint_revision=1), **{**scope, "operation_id": "old-source-wake", "expected": empty, "request_digest": "sha256:" + "1" * 64})
    except RuntimeError as error:
        assert error.args[0]["code"] == "E_STALE_EXECUTION", error
    else:
        raise AssertionError("deleted source admitted a wake demand")
    with sqlite3.connect(platform.root / "catalog/catalog.sqlite") as catalog:
        assert catalog.execute("SELECT count(*) FROM instance_bindings WHERE instance_id=?", (ref["instance_id"],)).fetchone() == (0,)
        assert catalog.execute("SELECT count(*) FROM network_attachments WHERE instance_id=? AND state<>'released'", (ref["instance_id"],)).fetchone() == (0,)
        stop_op, raw_heads = catalog.execute("SELECT stop_operation_id,private_heads_json FROM managed_retirements WHERE operation_id=?",
            (accepted["operation_id"],)).fetchone()
        (head,) = json.loads(raw_heads)
        assert stop_op == stopped["operation_id"], (stop_op, stopped)
        assert catalog.execute("SELECT count(*) FROM volume_heads WHERE volume_id=?", (head["volume_id"],)).fetchone() == (0,)

    def collected():
        with sqlite3.connect(platform.root / "catalog/catalog.sqlite") as catalog:
            return catalog.execute("SELECT state FROM resources WHERE resource_id=?", (head["head_resource_id"],)).fetchone() == ("deleted",)

    wait_for(collected, timeout=12)
    assert not (platform.root / "catalog/objects" / head["head_resource_id"]).exists(), head
    print("Manual Stop plus public Delete retired the parent across platform restart. Same-key replay returned the original receipt, and the old Worker scope could no longer wake.", flush=True)


def attach_for_retirement(platform, machine, volume, key):
    platform.complete(platform.change(machine + "/volume-attachments", "attach-" + key,
        dict(volume_id=volume["volume_id"], expected_volume_revision=volume["revision"], device_key="retained", mode="rw"),
        platform.app(machine)))
    return platform.complete(platform.change(machine + "/start", "boot-" + key, {}, platform.app(machine)))


def verify_volume_after_retirement(platform, volume, child_ref):
    marker = b"persistent-before-delete"
    read = ["/bin/dd", "if=/dev/vdb", "bs=1", f"count={len(marker)}", "status=none"]
    assert platform.guest(child_ref, read) == marker
    platform.guest(child_ref, ["/bin/sh", "-ec", "printf child-private-disk-only | dd of=/dev/vdb bs=1 conv=fsync status=none"])
    retained = platform.app("volumes/" + volume["volume_id"])
    assert retained["state"] == "available" and not retained.get("attachments"), retained
    choice = platform.complete(platform.change("machines", "replacement-for-retired-volume", dict(name="Detached volume consumer",
        template_id="t_system", boot_mode="system", runtime_instance_id=platform.serve_id, start=False,
        resources=dict(cpu_count=2, memory_mib=512))))
    target = choice["result"]["machine_id"]
    attach_for_retirement(platform, "machines/" + target, retained, "replacement")
    _, ref = platform.context(target)
    assert wait_for(lambda: platform.guest(ref, read), retry=(OSError, RuntimeError)) == marker
    print("The retained persistent volume attached to a new KVM guest with its original bytes. A write to the surviving child’s independent disk stayed private.", flush=True)
    unused = platform.complete(platform.change("machines", "never-started-delete", dict(name="Unused System slot",
        template_id="t_system", boot_mode="system", runtime_instance_id=platform.serve_id, start=False,
        resources=dict(cpu_count=1, memory_mib=256))))
    unused_path = "machines/" + unused["result"]["machine_id"]
    empty = platform.app(unused_path)
    removed = platform.complete(platform.change(unused_path, "retire-unused", {}, empty, method="DELETE"))
    assert removed["result"]["execution_epoch"] == 0 and platform.app(unused_path)["desired_power"] == "deleted"
    print("An unstarted System slot also retired at epoch zero without allocating an execution.", flush=True)


def saved_wake_failure_case(platform, source, ref, *, exact_only=False):
    def root_volume(execution):
        inventory = platform.native(f"/instances/{execution['instance_id']}/inventory")
        attachments = [d["block"]["managed_attachment"] for d in inventory["inventory"]["devices"] if d.get("block")]
        root, = [a for a in attachments if a["role"] == "root"]
        assert root["volume_id"] and root["execution_epoch"] == execution["execution_epoch"], root
        return root["volume_id"]

    original_root = root_volume(ref)
    disk_marker = os.urandom(16).hex()
    platform.guest(ref, ["/bin/sh", "-ec", 'printf original-saved-memory >/run/saved-wake-failure; '
                         'printf %s "$1" > /root/saved-wake-disk; sync /root/saved-wake-disk', "marker", disk_marker])
    hibernated = platform.complete(platform.change(source + "/hibernate", "save-for-failed-wake", {}, platform.app(source)))
    point = hibernated["result"]["statepoint"]
    original = platform.app(source)
    assert original["power_state"] == "hibernated" and original["binding"]["last_capture_id"] == point["capture_id"], original
    catalog_file = platform.root / "catalog/catalog.sqlite"
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id)
    manifest = platform.root / "catalog/statepoints" / point["representation_id"] / "CaptureManifest.json"
    withheld = manifest.with_name("CaptureManifest.fixture-withheld")

    def request_lease(key):
        return platform.app(source + "/leases", "POST", dict(reason="Exact saved wake", ttl_seconds=60, semantics_version=2),
            {"If-Control-Epoch": platform.epoch, "If-Match": f'"{original["revision"]}"', "Idempotency-Key": key}, 409, timeout=55)

    def check_held(epoch):
        empty = {**ref, "incarnation_id": None, "execution_epoch": epoch}
        status = worker(platform.worker, "GetLifecycleStatus", expected=empty, **scope)
        assert (status["power"], status["statepoint"], status["execution_epoch"], status["incarnation_id"]) == (
            "HIBERNATED", point["capture_id"], epoch, None), status
        assert status["effective_policy"]["wake_conditions"]["allow_cold_start"], status
        assert status["last_decision"]["kind"] == "vm_restore" and status["last_decision"]["stage"] == "aborted", status
        view = platform.app(source)
        assert view["power_state"] == "hibernated" and view["binding"]["last_capture_id"] == point["capture_id"], view
        assert not platform.app(source + "/leases")["items"]
        with sqlite3.connect(catalog_file) as db:
            assert db.execute("SELECT count(*) FROM operations WHERE instance_id=? AND kind='launch'", (ref["instance_id"],)).fetchone() == (1,)
        return empty, status["last_decision"]["operation_id"]

    manifest.rename(withheld)
    try:
        rejected = request_lease("unreadable-saved-wake")
        assert rejected["error"]["code"] == "STATEPOINT_RESTORE_FAILED", rejected
        _, failed = check_held(ref["execution_epoch"])
        result = platform.native("/operations/" + failed)
        assert result["phase"] == "ABORTED" and result["statepoint"]["error_code"] == "E_STATEPOINT_CORRUPT", result
        assert request_lease("unreadable-saved-wake")["error"]["code"] == "STATEPOINT_RESTORE_FAILED"
    finally:
        withheld.rename(manifest)
    print("An unreadable saved StatePoint rejected the public lease with a stable failure. Worker and Platform retained the original Hibernated cut even with cold wake authorized.", flush=True)

    def kill_unpublished_restore():
        deadline = time.monotonic() + 35
        with sqlite3.connect(catalog_file) as db:
            while time.monotonic() < deadline:
                entered = db.execute("""SELECT e.incarnation_id,e.pid,e.pid_start_time FROM executions e
                    JOIN operations o ON o.operation_id=e.last_operation_id WHERE e.instance_id=? AND e.execution_epoch=?
                    AND e.phase='registered' AND o.kind='restore_target' AND o.phase='NEW'
                    AND EXISTS(SELECT 1 FROM writer_grants g WHERE g.operation_id=o.operation_id
                        AND g.state IN ('offered','attached'))""",
                    (ref["instance_id"], ref["execution_epoch"] + 1)).fetchone()
                if entered:
                    incarnation, pid, start = entered
                    with os.fdopen(os.pidfd_open(pid), "rb", buffering=0) as process:
                        actual = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
                        assert actual == start
                        signal.pidfd_send_signal(process.fileno(), signal.SIGSTOP)
                        try:
                            activation = db.execute("""SELECT phase,decision,begin_revision FROM target_activations
                                WHERE incarnation_id=?""", (incarnation,)).fetchone()
                            assert not activation or activation in (("authority_prepared", "none", None),
                                                                  ("offered", "none", None)), activation
                        finally:
                            signal.pidfd_send_signal(process.fileno(), signal.SIGKILL)
                        assert select.select([process], [], [], 10)[0]
                        return
                time.sleep(0.005)
        raise TimeoutError("no held restore process with a writer grant")

    with ThreadPoolExecutor(max_workers=1) as clients:
        request = clients.submit(request_lease, "failed-restore-activation")
        kill_unpublished_restore()
        rejected = request.result(timeout=48)
    assert rejected["error"]["code"] == "STATEPOINT_RESTORE_FAILED", rejected
    advanced, failed = check_held(ref["execution_epoch"] + 1)
    result = platform.native("/operations/" + failed)
    assert result["phase"] == "ABORTED" and result["statepoint"]["capture_id"] == point["capture_id"], result
    with sqlite3.connect(catalog_file) as db:
        writer = db.execute("""SELECT g.state,e.phase,a.begin_revision,a.decision
            FROM writer_grants g JOIN executions e USING(incarnation_id)
            LEFT JOIN target_activations a ON a.operation_id=g.operation_id
            WHERE g.operation_id=? AND e.execution_epoch=g.execution_epoch""", (failed,)).fetchone()
        assert writer in (("released", "exited", None, None), ("released", "exited", None, "none")), writer
    if not exact_only:
        try:
            platform.native("/operations", dict(kind="launch", operation_id="forbidden-saved-cold-launch",
                request_digest="sha256:" + "3" * 64, expected=advanced, parameters=dict(name=original["runtime_name"],
                    boot_profile="system-debian-v1", cpus=1, memory_mib=512, init="/sbin/init", args=[],
                    disks=[dict(attachment_id="root", device_id="root", role="root", generation_id="fixture-system-root", read_only=False)])))
        except RuntimeError as error:
            _, status, body = error.args[0]
            assert status == 409 and json.loads(body)["code"] == "E_STATE_CONFLICT", error
        else:
            raise AssertionError("Worker accepted a cold launch over a saved execution")
    print("A restore VMM exited after receiving its writer grant and before Guest identity began. "
          "The Worker released the grant and retained the original saved cut.", flush=True)

    restored = platform.complete(platform.change(source + "/start", "repair-preserved-wake", {}, platform.app(source)))
    _, live = platform.context(original["machine_id"])
    assert live["execution_epoch"] == ref["execution_epoch"] + 2 and restored["result"]["statepoint"]["capture_id"] == point["capture_id"], restored
    assert wait_for(lambda: platform.guest(live, ["/bin/cat", "/run/saved-wake-failure"]), retry=(OSError, RuntimeError)) == b"original-saved-memory"
    assert root_volume(live) == original_root
    assert platform.guest(live, ["/bin/cat", "/root/saved-wake-disk"]).decode() == disk_marker
    after = worker(platform.worker, "GetLifecycleStatus", expected=live, **scope)
    assert after["power"] == "RUNNING" and after["statepoint"] is None, after
    print("Explicit exact retry recovered the original StatePoint, RAM and root volume", original_root,
          "at epoch", live["execution_epoch"], flush=True)


def statepoint_case(platform, source, ref, probe, seen_cids):
    peer = source_peer(platform, ref)
    view = platform.app(source)
    saved = platform.complete(platform.change(source + "/snapshots", "checkpoint", {
        "capture_mode": "vm", "label": "System checkpoint",
    }, view))
    assert saved["source_disposition"] == "RUNNING"
    assert saved["result"]["statepoint"]["phase"] == "durable"
    assert_fenced(peer)
    assert int(platform.guest(ref, [probe, "--cid"])) in seen_cids
    restored = platform.complete(platform.change("restores", "restore", {
        "snapshot_id": saved["result"]["snapshot_id"], "name": "Restored Debian",
        "volume_restore_policy": "clone_conflicts",
    }))
    _, child = platform.context(restored["machine_id"])
    inherited_agent(platform, child, probe, seen_cids)
    print("A durable System StatePoint restored with an independent Agent socket and CID.")


def hibernate_case(platform, source, ref, probe, seen_cids):
    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    original = os.pidfd_open(inventory["execution"]["pid"])
    try:
        hibernated = platform.complete(platform.change(source + "/hibernate", "hibernate", {},
                                                        platform.app(source)))
        assert hibernated["source_disposition"] == "EXITED"
        assert select.select([original], [], [], 5)[0], "hibernated source still alive"
    finally:
        os.close(original)
    platform.restart()
    woke = platform.complete(platform.change(source + "/start", "start-hibernated", {},
                                              platform.app(source)))
    assert woke["result"]["execution_epoch"] == ref["execution_epoch"] + 1
    _, successor = platform.context(platform.app(source)["machine_id"])
    assert successor["incarnation_id"] != ref["incarnation_id"]
    inherited_agent(platform, successor, probe, seen_cids)
    print("System hibernate fenced the old VMM and restored the Agent on a new CID.")


def packet_wake_case(platform, source_id, source_ref, binary, target_mode, *, boundaries=False, activity=False):
    serial = 0
    port = 45713
    source_attachment = platform.app(f"machines/{source_id}/network")
    source_mode = "fixed_internal" if target_mode == "native" else "native"
    assert source_attachment["state"] == "published" and source_attachment["mode"] == source_mode, source_attachment

    def call(method, ref, payload=None):
        nonlocal serial
        serial += 1
        key = f"packet-lifecycle-{serial}"
        scope = {k: ref[k] for k in ("instance_id", "incarnation_id", "execution_epoch")}
        return worker(platform.worker, method, payload, route_id="deep", bundle_digest=platform.bundle_id,
                      operation_id=key, request_digest="sha256:" + hashlib.sha256(key.encode()).hexdigest(),
                      expected=scope)

    def target(phase):
        created = platform.change("machines", "create-" + phase, dict(
            name="Packet destination " + phase, template_id="t_system", boot_mode="system",
            runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=1, memory_mib=512),
            network=dict(network_id="packet-net", mode=target_mode),
        ))
        platform.complete(created)
        machine_id = created["machine_id"]
        machine, ref = platform.context(machine_id)
        wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
                 retry=(RuntimeError, OSError))
        probe = platform.place_probe(ref, binary)
        platform.guest(ref, ["/usr/bin/systemd-run", "--unit=packet-wake-service", probe, "--serve", str(port)])
        wait_for(lambda: f":{port:04X}".encode() in platform.guest(ref, ["/bin/cat", "/proc/net/tcp"]))
        endpoint = platform.app(f"machines/{machine_id}/network")
        assert endpoint["state"] == "published" and endpoint["mode"] == target_mode, endpoint
        return f"machines/{machine_id}", machine, ref, endpoint

    def policy(machine, ref, allowed, expected_revision):
        nonlocal serial
        manifest = json.loads((platform.bundle / "runtime-bundle.json").read_text())
        if manifest["contracts"].get("lifecycle_policy") == 1:
            settings = platform.app(machine + "/lifecycle")
            if settings["supported"]:
                serial += 1
                return platform.complete(platform.change(machine, f"packet-policy-{serial}", dict(
                    lifecycle_policy=dict(wake_conditions=dict(private_network_packet=allowed)),
                    expected_policy_revision=settings["desired_revision"],
                ), platform.app(machine), method="PATCH"))
        return call("ApplyLifecyclePolicy", ref, dict(effective_policy=dict(
            wake_conditions=dict(private_network_packet=allowed),
        ), expected_applied_revision=expected_revision, new_revision=expected_revision + 1))

    def get_binding(machine):
        return platform.native(f"/machines/{machine['runtime_name']}/binding")

    def update_rule(path, key, enabled, revision):
        machine = platform.app(path.split("/network/", 1)[0])
        return platform.change(path, key, dict(enabled=enabled, expected_revision=revision), machine,
                               method="PUT", status=200)

    def send(ip, transport="tcp", destination=port, limit=15):
        command = ['/usr/bin/timeout', str(limit), '/bin/bash', '-c',
                   f'cat < /dev/tcp/{ip}/{destination}' if transport == 'tcp' else
                   f'echo wake > /dev/udp/{ip}/{destination}']
        try:
            return platform.guest(source_ref, command)
        except RuntimeError as error:
            return str(error).encode()

    def status(ref):
        return call("GetLifecycleStatus", ref)["power"]

    def authorized(path, epoch):
        decision = platform.app(path)["last_wake"]
        return decision and decision["authorized"] and decision["execution_epoch"] == epoch

    def expect_tcp(address, ref, limit=15):
        answer = send(address, limit=limit)
        if answer != b"private wake\n":
            diagnostics = []
            for guest in (source_ref, ref):
                with suppress(RuntimeError, OSError):
                    diagnostics.append(platform.guest(guest, ["/bin/sh", "-c", "ip -4 addr; ip -4 route; cat /proc/net/tcp"]))
            raise AssertionError((answer, diagnostics))

    def hibernate_boundary_case():
        machine, view, ref, endpoint = target("hibernate-cut")
        address = endpoint["private_ipv4"]
        rule_path = f"{machine}/network/wake-rules/{source_id}/tcp/{port}"
        policy(machine, ref, True, 0)
        assert update_rule(rule_path, "allow-hibernate-cut-peer", True, 0)["source_current"]
        db_path = f"file:{platform.root}/catalog/catalog.sqlite?mode=ro"

        def observe(sql, *params):
            with sqlite3.connect(db_path, uri=True) as db:
                return db.execute(sql, params).fetchone()

        def active(instance):
            return observe("""SELECT o.operation_id,b.original,b.stage,p.phase
                FROM instance_bindings i JOIN operations o ON o.operation_id=i.active_operation_id
                JOIN managed_fork_batches b USING(operation_id) LEFT JOIN statepoints p USING(capture_id)
                WHERE i.route_id='deep' AND i.instance_id=? AND o.kind='hibernate' AND o.phase='NEW'""", instance)

        def current_wake():
            return (platform.app(rule_path)["last_wake"] or {}).get("operation_id")

        def allowed_again(instance):
            row = observe("SELECT next_request_at FROM private_packet_wake_limits WHERE instance_id=?", instance)
            return row is None or row[0] <= time.clock_gettime_ns(time.CLOCK_BOOTTIME) // 1_000_000

        def arrange(original, label):
            previous = get_binding(view)
            if current_wake():
                # Retire late SYNs from the preceding connection before starting a new cut.
                time.sleep(3.2)
            wait_for(lambda: allowed_again(previous["instance_id"]), timeout=9)
            if original == "STANDBY":
                platform.complete(platform.change(machine + "/standby", "park-" + label, {}, platform.app(machine)))
                assert status(previous) == "STANDBY"
            accepted = platform.change(machine + "/hibernate", "hibernate-" + label, {}, platform.app(machine))
            operation_id, initial, stage, point = wait_for(lambda: active(previous["instance_id"]), timeout=30)
            assert initial == original and point not in ("stop_committed", "hibernated"), (initial, stage, point)
            return previous, accepted, operation_id

        for original in ("RUNNING", "STANDBY"):
            old_wake = current_wake()
            previous, accepted, native_op = arrange(original, "before-" + original.lower())
            expect_tcp(address, previous, limit=33)

            def aborted():
                receipt = platform.native(f"/operations/{native_op}")
                assert receipt["phase"] != "COMPLETED", receipt
                if receipt["phase"] == "ABORTED":
                    return receipt

            wait_for(aborted, timeout=45)
            wait_for(lambda: (op := current_wake()) and op != old_wake and authorized(rule_path, previous["execution_epoch"]), timeout=12)
            assert get_binding(view)["incarnation_id"] == previous["incarnation_id"]
            assert status(previous) == "RUNNING"
            wait_for(lambda: platform.app("operations/" + accepted["operation_id"])["state"] in ("failed", "cancelled"), timeout=30)
            print(f"An authorized private SYN before STOP_COMMIT aborted Hibernate from {original} and preserved its epoch.", flush=True)

        for original in ("RUNNING", "STANDBY"):
            old_wake = current_wake()
            previous, accepted, native_op = arrange(original, "after-" + original.lower())
            cursor = max((event["seq"] for event in call("QueryLifecycleEvents", previous, {"after": 0})["events"]), default=0)

            def committed():
                phase = observe("""SELECT p.phase FROM managed_fork_batches b
                    JOIN statepoints p USING(capture_id) WHERE b.operation_id=?""", native_op)
                return phase and phase[0] in ("stop_committed", "hibernated")

            wait_for(committed, timeout=90)
            with ThreadPoolExecutor(max_workers=2) as clients:
                answers = list(clients.map(lambda _: send(address, limit=38), range(2)))
            assert answers == [b"private wake\n", b"private wake\n"], answers
            platform.complete(accepted)
            successor = get_binding(view)
            assert successor["execution_epoch"] == previous["execution_epoch"] + 1
            wait_for(lambda: (op := current_wake()) and op != old_wake and authorized(rule_path, successor["execution_epoch"]), timeout=12)
            receipt = platform.native(f"/operations/{current_wake()}")
            assert receipt["statepoint"]["stage"] == "released", receipt
            events = call("QueryLifecycleEvents", successor, {"after": cursor})["events"]
            assert {event["operation_id"] for event in events if event["origin"] == "wake"} == {current_wake()}, events
            print(f"Two private SYNs after {original} STOP_COMMIT shared one exact restore at the next epoch.", flush=True)

    if boundaries:
        hibernate_boundary_case()
        return

    if activity:
        source = "machines/" + source_id
        policy(source, source_ref, True, 0)
        _, _, peer, endpoint = target("activity-peer")
        sse_port = port + 1
        path = "/run/private-activity-response"
        platform.guest(peer, ["/usr/bin/systemd-run", "--unit=private-activity-listener",
                              "/tmp/managed-vsock-probe", "--serve-sse", str(sse_port)])
        wait_for(lambda: f":{sse_port:04X}".encode() in platform.guest(peer, ["/bin/cat", "/proc/net/tcp"]))
        assert call("QueryHostActivity", source_ref)["outbound"] == 0
        assert call("QueryHostActivity", peer)["inbound"] == 0
        platform.guest(source_ref, ["/usr/bin/systemd-run", "--unit=private-activity-client", "/bin/bash", "-c",
            r"""exec 8<>"/dev/tcp/$1/$2"; printf 'GET /stream HTTP/1.1\r\nHost: peer\r\n\r\n' >&8; cat <&8 > "$3" """,
            "activity", endpoint["private_ipv4"], str(sse_port), path])
        wait_for(lambda: call("QueryHostActivity", source_ref)["outbound"] > 0
                 and call("QueryHostActivity", peer)["inbound"] > 0, timeout=8)
        wait_for(lambda: "host:outbound" in call("GetLifecycleStatus", source_ref)["blockers"], timeout=12)
        response = platform.guest(source_ref, ["/bin/cat", path])
        assert b"Content-Type: text/event-stream" in response and b"data: " not in response, response
        assert status(source_ref) == "RUNNING"
        wait_for(lambda: b"data: finished" in platform.guest(source_ref, ["/bin/cat", path]), timeout=25)
        wait_for(lambda: call("QueryHostActivity", source_ref)["outbound"] == 0
                 and call("QueryHostActivity", peer)["inbound"] == 0, timeout=7)
        wait_for(lambda: "host:outbound" not in call("GetLifecycleStatus", source_ref)["blockers"], timeout=12)
        print("A Guest-initiated private SSE registered real outbound and inbound activity during the silent wait. The idle listener and the closed stream had zero active connections.", flush=True)
        return

    machine, view, ref, endpoint = target("standby")
    address = endpoint["private_ipv4"]
    rule_path = f"{machine}/network/wake-rules/{source_id}/tcp/{port}"
    policy(machine, ref, False, 0)
    assert update_rule(rule_path, "allow-standby-peer", True, 0)["source_current"]
    assert platform.app(rule_path)["revision"] == 1
    source_endpoint = call("QueryPrivatePacketWakeEndpoint", source_ref)
    try:
        call("ApplyPrivatePacketWakeRule", ref, dict(source_attachment_id=source_endpoint["attachment_id"],
            target_attachment_id=endpoint["attachment_id"], transport="tcp", guest_port=port, enabled=True,
            expected_revision=1, source_execution={**source_ref, "execution_epoch": source_ref["execution_epoch"] + 1}))
    except RuntimeError as error:
        assert error.args[0]["code"] == "E_STALE_EXECUTION", error
    else:
        raise AssertionError("next source epoch was authorized by the current lease")
    expect_tcp(address, ref)
    platform.complete(platform.change(machine + "/standby", "standby-on-peer", {}, platform.app(machine)))
    assert status(ref) == "STANDBY"
    send(address, limit=4)
    assert status(ref) == "STANDBY", "a disabled lifecycle trigger caused wake"
    policy(machine, ref, True, 1)
    send(address, "udp")
    send(address, destination=port + 1, limit=4)
    assert status(ref) == "STANDBY", "a different destination port or transport caused wake"
    expect_tcp(address, ref)
    assert status(ref) == "RUNNING" and get_binding(view)["execution_epoch"] == ref["execution_epoch"]
    events = call("QueryLifecycleEvents", ref, dict(after=0))["events"]
    assert len({event["operation_id"] for event in events if event["origin"] == "wake"}) == 1, events
    wait_for(lambda: authorized(rule_path, ref["execution_epoch"]), timeout=8)
    print("Private TCP woke a real standby VMM once. Policy off, another port, and UDP left it suspended.")

    machine, view, ref, endpoint = target("hibernate")
    address = endpoint["private_ipv4"]
    rule_path = f"{machine}/network/wake-rules/{source_id}/tcp/{port}"
    policy(machine, ref, True, 0)
    assert update_rule(rule_path, "allow-hibernated-peer", True, 0)["source_current"]
    platform.complete(platform.change(machine + "/hibernate", "durable-packet-sleep", {}, platform.app(machine)))
    asleep = get_binding(view)
    assert asleep["incarnation_id"] is None and status(asleep) == "HIBERNATED"
    reserved = platform.app(machine + "/network")
    assert reserved["private_ipv4"] == address and reserved["dns_name"] == endpoint["dns_name"] and reserved["state"] == "reserved"
    assert not update_rule(rule_path, "revoke-sleeping-peer", False, 1)["enabled"]
    send(address, limit=4)
    assert status(asleep) == "HIBERNATED", "revoked rule caused durable wake"
    assert update_rule(rule_path, "reauthorize-sleeping-peer", True, 2)["source_current"]
    resolved = platform.guest(source_ref, ["/usr/bin/getent", "ahostsv4", endpoint["dns_name"]])
    assert address.encode() in resolved
    assert status(asleep) == "HIBERNATED", "DNS alone caused wake"
    expect_tcp(address, get_binding(view), limit=31)
    awake = get_binding(view)
    assert awake["incarnation_id"] != ref["incarnation_id"] and awake["execution_epoch"] == ref["execution_epoch"] + 1
    rebound = call("QueryPrivatePacketWakeEndpoint", awake)
    assert rebound["private_ipv4"] == address and rebound["attachment_id"] == endpoint["attachment_id"]
    events = call("QueryLifecycleEvents", awake, dict(after=0))["events"]
    assert len({event["operation_id"] for event in events if event["origin"] == "wake"}) == 1, events
    wait_for(lambda: authorized(rule_path, awake["execution_epoch"]), timeout=8)
    print("A private SYN restored a durable VM at its reserved address and a new execution epoch.")

    source_binary = platform.place_probe(source_ref, binary)
    for label, size, exit_code, expected in (("small", 96, 0, "forwarded"), ("oversized", 1300, 2, "dropped")):
        machine, view, ref, endpoint = target("udp-" + label)
        address = endpoint["private_ipv4"]
        udp_path = f"{machine}/network/wake-rules/{source_id}/udp/{port}"
        platform.guest(ref, ["/usr/bin/systemd-run", "--unit=packet-wake-udp",
                             "/tmp/managed-vsock-probe", "--serve-udp", str(port)])
        wait_for(lambda: f":{port:04X}".encode() in platform.guest(ref, ["/bin/cat", "/proc/net/udp"]))
        policy(machine, ref, True, 0)
        assert update_rule(udp_path, "allow-datagram-peer-" + label, True, 0)["source_current"]
        original_epoch = ref["execution_epoch"]
        platform.complete(platform.change(machine + "/hibernate", "durable-udp-" + label, {}, platform.app(machine)))
        asleep = get_binding(view)
        assert asleep["incarnation_id"] is None and asleep["execution_epoch"] == original_epoch
        result = platform.guest(source_ref, [source_binary, "--exchange-udp", address, str(port), str(size)], expected=exit_code)
        if expected == "forwarded":
            assert result == b"one datagram returned\n"
        awake = get_binding(view)
        assert awake["execution_epoch"] == original_epoch + 1

        def datagram_outcome():
            observed = platform.app(udp_path)["last_wake"]
            return observed and observed["authorized"] and observed["datagram"] == expected \
                and observed["execution_epoch"] == awake["execution_epoch"]

        wait_for(datagram_outcome, timeout=8)
    print("One authorized UDP datagram crossed durable wake. Oversized data woke the VM and was dropped.")
    print(f"Private packet round trip {source_mode} -> {target_mode} on {platform.bundle_id}.", flush=True)


class ResponseBytes:
    def __init__(self, data):
        self.data = data

    def makefile(self, *_args, **_kwargs):
        return io.BytesIO(self.data)


def parse_http(data):
    response = HTTPResponse(ResponseBytes(data))
    response.begin()
    return response.status, json.loads(response.read())


def relay_case(platform, source, machine_id, ref, probe):
    expires = datetime.now(timezone.utc) + timedelta(minutes=5)
    grant = platform.change("guest-grants", "grant", dict(
        source_machine_id=machine_id, target_machine_id=machine_id, action="machine.exec",
        expires_at=expires.isoformat(), maximum_uses=1,
    ), status=200)
    issue = f"guest-grants/{grant['grant_id']}/credential"
    token = platform.change(issue, "token", {"ttl_seconds": 240}, status=200)["token"]
    assert "token" not in platform.change(issue, "token", {"ttl_seconds": 240}, status=200)
    body = json.dumps({"command": ["/bin/echo", "delegated"], "timeout_seconds": 10})
    wire = (f"POST /v1/{source}/exec HTTP/1.1\r\nHost: guest-relay\r\nAuthorization: Bearer {token}\r\n"
            f"Content-Type: application/json\r\nContent-Length: {len(body.encode())}\r\n"
            "X-Guest-CID: 42\r\nConnection: close\r\n\r\n" + body)
    path = platform.relay_dir / (ref["incarnation_id"] + ".sock")
    wait_for(path.is_socket)
    with socket.socket(socket.AF_UNIX) as host:
        host.settimeout(8)
        host.connect(str(path))
        host.sendall(wire.encode())
        assert parse_http(host.makefile("rb").read())[0] == 401
    request(HTTPConnection("127.0.0.1", platform.port, timeout=10), "GET", "/v1/" + source,
            headers={"Authorization": "Bearer " + token}, status=401)

    def run_exec():
        return parse_http(platform.guest(ref, [probe], wire))

    status, result = run_exec()
    assert status == 200 and base64.b64decode(result["stdout"]) == b"delegated\n"
    assert run_exec()[0] == 403
    platform.app(f"guest-grants/{grant['grant_id']}", "DELETE", {}, {
        "If-Control-Epoch": platform.epoch, "Idempotency-Key": "revoke", "If-Match": '"1"',
    })
    assert run_exec()[0] == 401
    print("The real Guest relay enforced measured peer identity, one exec use, and revocation.")


def grant_rollback_case(platform, source, machine_id, ref, probe):
    def grant(key):
        return platform.change("guest-grants", key, {
            "source_machine_id": machine_id, "target_machine_id": machine_id, "action": "machine.exec",
            "expires_at": (datetime.now(timezone.utc) + timedelta(minutes=10)).isoformat(), "maximum_uses": 2,
        }, status=200)

    def record(identifier):
        return next(row for row in platform.app("guest-grants")["items"] if row["grant_id"] == identifier)

    marker = os.urandom(12).hex()
    platform.guest(ref, ["/bin/sh", "-ec", 'printf %s "$1" >/run/grant-rollback', "write", marker])
    old_id = grant("grant-at-checkpoint")["grant_id"]
    assert not record(old_id).get("revoked_at")
    point = platform.complete(platform.change(source + "/snapshots", "before-revocation", {
        "capture_mode": "vm", "label": "Owner grant enabled",
    }, platform.app(source)))
    assert point["result"]["statepoint"]["phase"] == "durable", point
    snapshot = point["result"]["snapshot_id"]
    platform.guest(ref, ["/bin/sh", "-ec", "echo changed > /run/grant-rollback"])
    revoked = platform.app(f"guest-grants/{old_id}", "DELETE", {}, {
        "If-Control-Epoch": platform.epoch, "Idempotency-Key": "owner-revoke-before-rollback", "If-Match": '"1"',
    })
    stored = record(old_id)
    assert revoked["status"] == "revoked" and stored["revision"] == 2 and stored["revoked_at"]
    rollback = platform.complete(platform.change(source + "/rollback", "restore-earlier-machine", {
        "snapshot_id": snapshot, "volume_restore_policy": "clone_conflicts",
    }, platform.app(source)))
    _, active = platform.context(machine_id)
    assert active["instance_id"] == ref["instance_id"] and active["execution_epoch"] > ref["execution_epoch"]
    assert active["incarnation_id"] != ref["incarnation_id"] and record(old_id) == stored
    assert wait_for(lambda: platform.guest(active, ["/bin/cat", "/run/grant-rollback"]) == marker.encode(),
                    retry=(RuntimeError, OSError))
    platform.restart()
    assert record(old_id) == stored
    fresh = grant("grant-on-restored-execution")["grant_id"]
    token = platform.change(f"guest-grants/{fresh}/credential", "current-credential", {"ttl_seconds": 180}, status=200)["token"]
    body = json.dumps({"command": ["/bin/echo", marker], "timeout_seconds": 10})
    wire = (f"POST /v1/{source}/exec HTTP/1.1\r\nHost: guest-relay\r\nAuthorization: Bearer {token}\r\n"
            f"Content-Type: application/json\r\nContent-Length: {len(body.encode())}\r\nConnection: close\r\n\r\n" + body)
    status, result = parse_http(platform.guest(active, [probe], wire))
    assert status == 200 and base64.b64decode(result["stdout"]).strip() == marker.encode()
    assert record(fresh)["used_count"] == 1 and record(old_id) == stored
    print(json.dumps({"bundle": platform.bundle_id, "snapshot": snapshot, "operation": rollback["operation_id"],
                      "original_epoch": ref["execution_epoch"], "restored_epoch": active["execution_epoch"],
                      "revoked_revision": stored["revision"], "new_grant_uses": 1, "workload_marker": marker}),
          flush=True)


def protected_fork_case(platform, source, ref, probe, seen_cids):
    status = platform.app(source + "/lifecycle")
    policy = {"auto_standby": False, "auto_hibernate": False, "keep_running": True}
    platform.complete(platform.change(source, "protect-fork", {"expected_policy_revision": status["desired_revision"],
        "lifecycle_policy": policy}, platform.app(source), method="PATCH"))
    headers = {"If-Control-Epoch": platform.epoch, "If-Match": f'"{platform.app(source)["revision"]}"', "Idempotency-Key": "protect-lease"}
    lease = platform.app(source + "/leases", "POST", {"reason": "waiting for a response", "semantics_version": 2, "ttl_seconds": 180}, headers)
    assert lease["state"] == "active"
    # A blocked user process keeps a nonce exclusively in RAM. Each branch
    # gets a different message through its own copy of the FIFO after release.
    wait_script = """nonce=$(cat /proc/sys/kernel/random/uuid)
printf '%s %s\\n' "$$" "$nonce" > /run/fork-wait-id
IFS= read -r reply < /run/fork-wait-pipe
printf '%s %s %s\\n' "$$" "$nonce" "$reply" > /run/fork-wait-result"""
    platform.guest(ref, ["sh", "-c", 'mkfifo /run/fork-wait-pipe && sh -c "$1" </dev/null >/dev/null 2>&1 &', "launcher", wait_script])
    identity = wait_for(lambda: platform.guest(ref, ["cat", "/run/fork-wait-id"]).strip(), retry=(RuntimeError, OSError))
    children = fork_case(platform, source, ref, probe, seen_cids)
    for child in children:
        path = "machines/" + child["machine_id"]
        child_policy = platform.app(path + "/lifecycle")
        assert child_policy["source"] == "parent" and all(child_policy["desired"][k] == v for k,v in policy.items()), child_policy
        assert not platform.app(path + "/leases")["items"]
    for branch, execution in [("source", ref), *((child["machine_id"], platform.context(child["machine_id"])[1]) for child in children)]:
        platform.guest(execution, ["sh", "-ec", 'printf "%s\\n" "$1" > /run/fork-wait-pipe', "reply", branch])
        result = wait_for(lambda: platform.guest(execution, ["cat", "/run/fork-wait-result"]).strip(), retry=(RuntimeError, OSError))
        assert result == identity + b" " + branch.encode(), result
    print("A protected System guest forked while its user task waited. Both children resumed the same PID and nonce, inherited policy, and had independent leases and replies.", flush=True)


def connection_recovery_case(platform, source, machine_id, ref, probe):
    revision = platform.app(source)["revision"]
    headers = {"If-Control-Epoch": platform.epoch, "If-Match": f'"{revision}"', "Idempotency-Key": "separate-lease"}
    lease = platform.app(source + "/leases", "POST", {"reason": "independent owner", "semantics_version": 2, "ttl_seconds": 180}, headers)
    grant = platform.change("guest-grants", "long-exec", dict(
        source_machine_id=machine_id, target_machine_id=machine_id, action="machine.exec", maximum_uses=1,
        expires_at=(datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat()), status=200)
    token = platform.change(f"guest-grants/{grant['grant_id']}/credential", "grant-token", {"ttl_seconds": 180}, status=200)["token"]
    body = json.dumps({"command": ["/bin/sleep", "25"], "timeout_seconds": 40})
    wire = (f"POST /v1/{source}/exec HTTP/1.1\r\nHost: guest-relay\r\nAuthorization: Bearer {token}\r\n"
            f"Content-Length: {len(body.encode())}\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n" + body)

    def forward():
        with suppress(OSError, RuntimeError):
            platform.guest(ref, [probe], wire)

    thread = threading.Thread(target=forward, daemon=True)
    thread.start()
    database = platform.root / "platform/platform.db"

    def owned_row():
        with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as db:
            return db.execute("SELECT id FROM connection_activity WHERE principal=?", (grant["grant_id"],)).fetchone()

    hold, = wait_for(owned_row, timeout=15)
    scope = dict(route_id="deep", bundle_digest=platform.bundle_id, expected=ref)

    def protection(method="QueryProtection", ident=hold, **extra):
        args = dict(protection_id=ident, principal=grant["grant_id"], **extra)
        request = dict(scope)
        if method != "QueryProtection":
            request.update(operation_id="fixture-" + method + "-" + ident,
                           request_digest="sha256:" + hashlib.sha256(json.dumps(args, sort_keys=True).encode()).hexdigest())
        return worker(platform.worker, method, args, **request)

    wait_for(lambda: protection()["state"] == "active", timeout=10, retry=(RuntimeError,))
    platform.restart()
    wait_for(lambda: protection()["state"] == "released" and not owned_row(), timeout=15)
    thread.join(timeout=2)
    leases = platform.app(source + "/leases")["items"]
    assert any(item["lease_id"] == lease["lease_id"] and item["state"] == "active" for item in leases), leases
    delayed = "access-" + os.urandom(16).hex()
    assert protection("CloseAccessProtection", delayed)["state"] == "released"
    try:
        protection("AcquireActivityHold", delayed, reason="late connection request")
    except RuntimeError as error:
        assert error.args[0]["code"] == "E_STALE_REVISION", error
    else:
        raise AssertionError("a delayed registration recreated a closed connection")
    assert platform.context(machine_id)[1] == ref
    print("After SIGKILL, the replacement platform released only its old Guest connection. An independent lease survived and a late Worker admission stayed closed.", flush=True)


def cpu_limit_case(platform, source, machine_id, ref, historical):
    def quota(execution):
        pid = platform.native(f"/instances/{execution['instance_id']}/inventory")["execution"]["pid"]
        groups = Path(f"/proc/{pid}/cgroup").read_text().splitlines()
        group, = [line.removeprefix("0::/") for line in groups if line.startswith("0::/")]
        maximum, period = (Path("/sys/fs/cgroup") / group / "cpu.max").read_text().split()
        return (None if maximum == "max" else int(maximum)), int(period)

    original = quota(ref)
    prior = platform.app(source)
    request = {"resources": {"cpu_limit_millicpus": 250}}
    accepted = platform.change(source, "quarter-cpu", request, prior, method="PATCH")
    if historical:
        def rejected():
            op = platform.app("operations/" + accepted["operation_id"])
            return op if op["state"] in ("succeeded", "failed", "recovery_required") else None

        operation = wait_for(rejected)
        assert operation["state"] == "failed" and operation["error"]["code"] == "CAPABILITY_UNSUPPORTED", operation
        assert quota(ref) == original
        report = platform.app(source + "/resources")
        assert report["execution"] == ref and report["issues"]["cpu_limit"]["code"] == "CAPABILITY_UNSUPPORTED", report
        mixed = platform.change(source, "historical-mixed", {"keep_running": True, "resources": {"cpu_count": 1}},
                                platform.app(source), method="PATCH")
        partial = wait_for(lambda: (op if (op := platform.app("operations/" + mixed["operation_id"]))["state"] == "failed" else None))
        assert partial["effect"] == "partial" and partial["result"]["applied_lifecycle_revision"] > 0, partial
        assert platform.app(source)["keep_running"] and quota(ref) == original
        print("The historical Worker refused the public CPU limit request and kept the kernel quota.", flush=True)
        return

    receipt = platform.complete(accepted)["result"]["cpu_limit"]
    amount, period = quota(ref)
    assert amount * 1000 == 250 * period and (amount, period) == (receipt["quota_usec"], receipt["period_usec"])
    assert all(receipt[key] == value for key, value in ref.items())
    assert platform.change(source, "quarter-cpu", request, prior, method="PATCH")["operation_id"] == accepted["operation_id"]
    platform.complete(platform.change(source + "/hibernate", "quota-save", {}, platform.app(source)))
    platform.restart()
    platform.complete(platform.change(source + "/start", "quota-wake", {}, platform.app(source)))
    _, new = platform.context(machine_id)
    assert new["execution_epoch"] == ref["execution_epoch"] + 1 and new["incarnation_id"] != ref["incarnation_id"]
    amount, period = quota(new)
    assert amount * 1000 == 250 * period
    restored = platform.complete(platform.change(source, "clear-quota", {"resources": {"cpu_limit_millicpus": None}},
                                                  platform.app(source), method="PATCH"))["result"]["cpu_limit"]
    amount, period = quota(new)
    assert (amount, period) == original and restored["requested_millicpus"] is None, (original, restored)
    print("Public CPU limit and replay matched the kernel quota. Exact restore kept 250 millicpus. Clearing restored the original allocated quota.", flush=True)


def live_resources_case(platform, source, ref, hot):
    def status():
        report = platform.app(source + "/resources")
        assert report["execution"] == ref and report["compute"]["capture_compatible"] == (not hot), report
        assert report["compute"]["profile"] == ("live-compute" if hot else "parked-capture"), report
        return report

    def memory_kib():
        return int(platform.guest(ref, ["/bin/cat", "/proc/meminfo"]).splitlines()[0].split()[1])

    def patch(key, values):
        return platform.change(source, key, {"resources": values}, platform.app(source), method="PATCH")

    prior = platform.app(source)
    initial = status()
    before = initial["compute"]["observed"]
    assert before["online_cpus"] == (1 if hot else 2) and before["balloon"]["actual_mib"] == 0, before
    old_mem = memory_kib()
    target = {"cpu_count": 2 if hot else 1, "balloon_target_mib": 16}
    if hot:
        target["memory_mib"] = 640
        if "cpu_limit" in initial:
            target["cpu_limit_millicpus"] = 1500
    accepted = platform.change(source, "live-compute", {"resources": target}, prior, method="PATCH")
    result = platform.complete(accepted)["result"]
    assert len(result["changes"]) == len(target) - ("cpu_limit_millicpus" in target)
    assert all(p["stage"] == "completed" for p in result["changes"]), result
    assert result["execution"] == ref
    assert platform.change(source, "live-compute", {"resources": target}, prior, method="PATCH")["operation_id"] == accepted["operation_id"]
    report = status()
    observed = report["compute"]["observed"]
    assert all(report["requested"][k] == v for k, v in target.items()), report
    assert observed["online_cpus"] == target["cpu_count"] and observed["cpus"]["created"] == 2
    assert observed["balloon"] == {"actual_mib": 16, "target_mib": 16}, observed
    assert platform.guest(ref, ["/bin/cat", "/sys/devices/system/cpu/online"]).strip() == (b"0-1" if hot else b"0")
    if hot:
        assert observed["ram"]["mapped"] == observed["ram"]["plugged"] == 128 << 20, observed
        assert memory_kib() > old_mem + 96 * 1024
        source_view = platform.app(source)
        assert not source_view["capabilities"]["state.hibernate"]["supported"]
        denial = platform.change(source + "/hibernate", "incompatible-cut", {}, source_view, status=409)
        assert denial["error"]["code"] == "CAPABILITY_UNSUPPORTED", denial
        assert status()["execution"] == ref
        if "cpu_limit_millicpus" in target:
            quota = report["cpu_limit"]
            assert quota["requested_millicpus"] == 1500 and quota["quota_usec"] * 1000 == 1500 * quota["period_usec"], quota
    else:
        invalid = patch("unsupported-combination", {"cpu_count": 2, "memory_mib": 896})
        failed = wait_for(lambda: (op if (op := platform.app("operations/" + invalid["operation_id"]))["state"] == "failed" else None))
        assert failed["effect"] == "none" and failed["error"]["code"] == "CAPABILITY_UNSUPPORTED", failed
        assert status()["compute"]["observed"]["online_cpus"] == 1
    platform.restart()
    assert platform.complete(accepted)["result"] == result
    clear = {"balloon_target_mib": 0}
    if "cpu_limit_millicpus" in target:
        clear["cpu_limit_millicpus"] = None
    if not hot:
        clear["cpu_count"] = 2
    platform.complete(patch("return-resources", clear))
    final = status()["compute"]["observed"]
    assert final["online_cpus"] == 2 and final["balloon"]["actual_mib"] == final["balloon"]["target_mib"] == 0
    if "cpu_limit_millicpus" in target:
        quota = status()["cpu_limit"]
        assert quota["requested_millicpus"] is None and quota["quota_usec"] == 2 * quota["period_usec"], quota
        print("Verified 1500 millicpus and restored the two-slot kernel allocation.", flush=True)
    if not hot:
        driver = "/sys/bus/virtio/drivers/virtio_balloon"
        device = platform.guest(ref, ["/bin/sh", "-ec", 'set -- "$1"/virtio*; test -d "$1"; basename "$1"', "device", driver]).decode().strip()
        assert device.startswith("virtio")
        platform.guest(ref, ["/bin/sh", "-ec", 'printf %s "$1" > "$2/unbind"', "unbind", device, driver])
        pending = patch("recover-partial", {"cpu_count": 1, "balloon_target_mib": 12})
        def waiting_for_balloon():
            current = status()
            progress = current["compute"].get("pending") or {}
            return current if progress.get("change", {}).get("change") == "balloon" else None
        during = wait_for(waiting_for_balloon)
        assert during["requested"]["cpu_count"] == 1 and during["requested"]["balloon_target_mib"] == 0, during
        platform.restart()
        failure = wait_for(lambda: (op if (op := platform.app("operations/" + pending["operation_id"]))["state"] == "failed" else None))
        assert failure["effect"] == "partial" and failure["result"]["changes"][0]["change"]["cpus"] == 1, failure
        assert failure["result"]["rejected"]["stage"] == "aborted", failure
        report = status()
        assert report["requested"]["cpu_count"] == report["compute"]["observed"]["online_cpus"] == 1
        assert report["requested"]["balloon_target_mib"] == report["compute"]["observed"]["balloon"]["actual_mib"] == 0
        platform.guest(ref, ["/bin/sh", "-ec", 'printf %s "$1" > "$2/bind"', "bind", device, driver])
        print("After platformd was killed during balloon adjustment, its original operation reported the confirmed CPU change and the rejected balloon separately.", flush=True)
    print(f"Public {report['compute']['profile']} settings matched the live Linux CPU set, VMM balloon and durable operation after platform restart. Target {target}.", flush=True)
    print(f"Resource measurement {report['compute']['profile']} on {platform.bundle_id}.", flush=True)


def policy_case(platform, source, machine_id, execution):
    def change(path, method, key, body, revision, status=200, *, timeout=15):
        return platform.app(path, method, body, {"If-Control-Epoch": platform.epoch,
                            "If-Match": f'"{revision}"', "Idempotency-Key": key}, status, timeout=timeout)

    status = platform.app(source + "/lifecycle")
    assert status["supported"] and status["applied_revision"] == status["desired_revision"], status
    defaults = platform.app("system/settings")
    assert defaults["default_boot_mode"] == "oci" and platform.app("control")["default_boot_mode"] == "oci"
    values = {"auto_standby": True, "standby_after_seconds": 10, "auto_hibernate": True, "hibernate_after_seconds": 72}
    new = change("system/settings", "PATCH", "set-defaults", {"default_lifecycle_policy": values, "default_boot_mode": "system"}, defaults["revision"])
    assert new["default_boot_mode"] == "system" and platform.app("control")["default_boot_mode"] == "system"
    assert platform.app(source + "/lifecycle")["default_revision"] == status["default_revision"]
    view = platform.app(source)
    patch = {"expected_policy_revision": status["desired_revision"],
             "lifecycle_policy": {"keep_running": True, "wake_conditions": {"private_network_packet": False}}}
    accepted = platform.change(source, "configure", patch, view, method="PATCH")
    platform.complete(accepted)
    applied = platform.app(source + "/lifecycle")
    assert applied["applied_revision"] == status["applied_revision"] + 1
    assert applied["overrides"] == patch["lifecycle_policy"] and applied["applied"]["keep_running"], applied
    assert platform.change(source, "configure", patch, view, method="PATCH")["operation_id"] == accepted["operation_id"]
    current = platform.app(source)
    body = {"reason": "streaming response", "ttl_seconds": 60, "semantics_version": 2}
    lease = change(source + "/leases", "POST", "acquire", body, current["revision"])
    assert lease["execution_epoch"] == execution["execution_epoch"] and lease["state"] == "active"
    assert "lease_token" not in change(source + "/leases", "POST", "acquire", body, current["revision"])
    path = "leases/" + lease["lease_id"]
    renew = {"lease_token": lease["lease_token"], "ttl_seconds": 60}
    updated = change(path, "PATCH", "renew", renew, lease["revision"])
    assert updated["revision"] > lease["revision"]
    change(path, "PATCH", "stale-renew", renew, lease["revision"], 409)
    release = change(path, "DELETE", "release", {"lease_token": lease["lease_token"]}, updated["revision"])
    assert release["state"] == "released"
    selected = {"default_policy_revision": new["default_lifecycle_revision"], "machines": [
        {"machine_id": machine_id, "expected_revision": platform.app(source)["revision"]},
        {"machine_id": "m_missing_test", "expected_revision": 1}]}
    app = platform.change("lifecycle-policy-applications", "apply-list", selected, status=200)
    assert app["complete"] and [m["state"] for m in app["machines"]] == ["applied", "rejected"], app
    assert app["machines"][1]["reason"] == "MACHINE_NOT_FOUND"
    state = platform.app(source + "/lifecycle")
    assert state["overrides"] == {} and state["applied"]["hibernate_after_seconds"] == 72, state
    change("system/settings", "PATCH", "later-default", {"default_lifecycle_policy": {"hibernate_after_seconds": 99}, "default_boot_mode": "oci"}, new["revision"])
    platform.restart()
    assert platform.change("lifecycle-policy-applications", "apply-list", selected, status=200) == app
    assert platform.app("lifecycle-policy-applications/" + app["application_id"]) == app
    print("Public policy defaults, explicit overrides, per-machine receipts and renewable execution leases survived a platform restart.", flush=True)
    platform.complete(platform.change(source + "/hibernate", "policy-hibernate", {}, platform.app(source)))
    sleeping = platform.app(source)
    assert sleeping["power_state"] == "hibernated", sleeping
    change(source + "/leases", "POST", "offline-stale", body, sleeping["revision"] + 1, 409)
    assert platform.app(source)["power_state"] == "hibernated"
    with ThreadPoolExecutor(max_workers=2) as clients:
        concurrent = [clients.submit(change, source + "/leases", "POST", "offline-acquire", body, sleeping["revision"], timeout=105)
                      for _ in range(2)]
        replies = [request.result() for request in concurrent]
    assert len({item["lease_id"] for item in replies}) == 1 and sum("lease_token" in item for item in replies) == 1, [item["state"] for item in replies]
    after = next(item for item in replies if "lease_token" in item)
    assert after["state"] == "active" and after["execution_epoch"] == execution["execution_epoch"] + 1, after
    assert platform.app(source)["power_state"] == "running"
    assert "lease_token" not in change(source + "/leases", "POST", "offline-acquire", body, sleeping["revision"])
    change("leases/" + after["lease_id"], "DELETE", "release-after-wake", {"lease_token": after["lease_token"]}, after["revision"])
    print("An authorized lease woke the saved System execution once. A stale machine revision left it hibernated.", flush=True)
    before = platform.app(source + "/lifecycle")
    disabled = {"expected_policy_revision": before["desired_revision"], "lifecycle_policy": {
        "wake_conditions": {"api_access": False}, "auto_standby": False, "auto_hibernate": False}}
    platform.complete(platform.change(source, "disable-wake", disabled, platform.app(source), method="PATCH"))
    online = change(source + "/leases", "POST", "online-no-wake", body, platform.app(source)["revision"])
    assert online["execution_epoch"] == after["execution_epoch"], online
    change("leases/" + online["lease_id"], "DELETE", "close-online", {"lease_token": online["lease_token"]}, online["revision"])
    platform.complete(platform.change(source + "/hibernate", "manual-no-wake", {}, platform.app(source)))
    change(source + "/leases", "POST", "deny-offline", body, platform.app(source)["revision"], 403)
    assert platform.app(source)["power_state"] == "hibernated"
    print("With implicit API wake disabled, active guests still admitted leases and saved guests remained hibernated.", flush=True)


def test(args, root, stack, case):
    retirement = case == "task-adoption-delete"
    if case in ("task-adoption", "task-adoption-delete", "task-fork-cancel-retry"):
        os.environ["SMOLVM_WORKER_VCPU_BUDGET"] = "4"
    packet_case = case.startswith("packet-")
    networks = (dict(network_id="packet-net", route_domain_id="fixture-route", cidr="10.44.30.0/24"),) if packet_case or retirement else ()
    epoch_check = case == "polish-metrics-epoch"
    if epoch_check:
        networks = (dict(network_id="idle-net", route_domain_id="idle-route", cidr="10.64.39.0/24"),)
    platform = Platform(args, root, stack, networks, gateway={"apps_domain": "polish-idle.test"} if epoch_check else None,
                        state_tmpfs=case in ("saved-wake-failure", "saved-wake-exact", "grant-rollback"),
                        use_installation=case in INSTALLED_CASES)
    candidate = args.platformd
    if case == "polish-history":
        args.platformd = args.previous_platformd
    platform.start()
    intent = dict(
        name="System vsock fixture", template_id="t_system", boot_mode="system",
        runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=2, memory_mib=768),
    )
    if case in ("task-fork-cancel-retry", "fork-lost-reply", "grant-rollback"):
        intent["resources"] = dict(cpu_count=1, memory_mib=512)
    if epoch_check:
        intent.update(resources=dict(cpu_count=1, memory_mib=512), network=dict(network_id="idle-net", mode="native"),
                      lifecycle_policy=dict(auto_standby=False, auto_hibernate=False, min_running_after_wake_seconds=0,
                                            pressure_reclaim="disabled"))
    if case == "live-resources-hot":
        intent.update(live_compute_growth=True, resources=dict(cpu_count=1, memory_mib=512))
    if packet_case:
        intent["network"] = dict(network_id="packet-net", mode="fixed_internal" if case.endswith("-native") else "native")
        intent["resources"] = dict(cpu_count=1, memory_mib=512)
    if retirement:
        intent.update(start=False, network=dict(network_id="packet-net", mode="native"),
                      lifecycle_policy=dict(keep_running=True, wake_conditions=dict(allow_cold_start=True)))
    if case in ("saved-wake-failure", "saved-wake-exact"):
        intent.update(resources=dict(cpu_count=1, memory_mib=512), lifecycle_policy=dict(keep_running=True,
            retry_backoff_seconds=2, wake_conditions=dict(allow_cold_start=True, api_access=True)))
    created = platform.change("machines", "create", intent)
    platform.complete(created)
    machine_id = created["machine_id"]
    source = "machines/" + machine_id
    volume = None
    if retirement:
        volume = platform.complete(platform.change("volumes", "retained-volume", dict(name="Independent persistent volume",
            runtime_instance_id=platform.serve_id, capacity_bytes=32 << 20)))["result"]["volume"]
        attach_for_retirement(platform, source, volume, "source")
    _, ref = platform.context(machine_id)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    if epoch_check:
        from polish_metrics_epoch import check_passive_idle_epoch
        check_passive_idle_epoch(platform, source, ref)
        return
    if case == "polish-history":
        from polish_product import check_historical_capture
        check_historical_capture(platform, source, ref, candidate)
        return
    if case in ("polish-files", "polish-capture", "polish-lease-fork", "polish-metrics", "polish-old-agent"):
        from polish_product import check_capture, check_files, check_lease_and_fork, check_old_agent, check_passive_metrics
        check = {"polish-files": check_files, "polish-capture": check_capture,
                 "polish-lease-fork": check_lease_and_fork, "polish-metrics": check_passive_metrics,
                 "polish-old-agent": check_old_agent}[case]
        check(platform, source, ref)
        return
    if case in ("live-resources", "live-resources-hot"):
        live_resources_case(platform, source, ref, case == "live-resources-hot")
        return
    if retirement:
        platform.guest(ref, ["/bin/sh", "-ec", "printf persistent-before-delete | dd of=/dev/vdb bs=1 status=none"])
    if case in ("task-adoption", "task-adoption-delete", "task-restore", "task-fork-cancel-retry"):
        native_task_case(platform, source, ref, case, volume)
        return
    if case == "task-wake":
        task_wake_case(platform, source, ref)
        return
    if case == "fork-cancel":
        cancel_partial_fork(platform, source, ref)
        return
    if case == "fork-lost-reply":
        fork_after_lost_reply(platform, source, ref)
        return
    if case in ("saved-wake-failure", "saved-wake-exact"):
        saved_wake_failure_case(platform, source, ref, exact_only=case == "saved-wake-exact")
        return
    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    assert any(device["virtio_type"] == 19 and device.get("parked_vsock_rebind")
               for device in inventory["inventory"]["devices"])
    binary = vsock_probe_binary(args, root)
    if packet_case:
        packet_wake_case(platform, machine_id, ref, binary, "native" if case.endswith("-native") else "fixed_internal",
                         boundaries=case.startswith("packet-cut-"), activity=case == "packet-activity")
        return
    probe = platform.place_probe(ref, binary)
    seen = {int(platform.guest(ref, [probe, "--cid"]))}
    platform.guest(ref, ["/bin/sh", "-c", "printf 'held-source\\n' > /var/tmp/platform-cut-marker; sync"])
    if case != "relay":
        helper = platform.guest(ref, ["/usr/bin/sha256sum", "/sbin/smolvm-identity-helper"]).split()[0]
        assert helper.decode() == digest(platform.bundle / "guest/identity-helper").split(":")[1]
    if case in ("cpu-limit", "cpu-limit-historical"):
        cpu_limit_case(platform, source, machine_id, ref, case == "cpu-limit-historical")
    elif case == "protected-fork":
        protected_fork_case(platform, source, ref, probe, seen)
    elif case == "connection-recovery":
        connection_recovery_case(platform, source, machine_id, ref, probe)
    elif case == "policy":
        policy_case(platform, source, machine_id, ref)
    elif case == "relay":
        relay_case(platform, source, machine_id, ref, probe)
    elif case == "grant-rollback":
        grant_rollback_case(platform, source, machine_id, ref, probe)
    elif case == "fork":
        fork_case(platform, source, ref, probe, seen)
    elif case == "gate-without-agent":
        gate_without_agent(platform, source, ref, probe, seen)
    elif case == "statepoint":
        statepoint_case(platform, source, ref, probe, seen)
    elif case == "successive-captures":
        statepoint_case(platform, source, ref, probe, seen)
        hibernate_case(platform, source, ref, probe, seen)
    else:
        hibernate_case(platform, source, ref, probe, seen)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=lambda p: Path(p).resolve(), required=True)
    parser.add_argument("--platformd", type=lambda p: Path(p).resolve(), required=True)
    parser.add_argument("--system-image", type=lambda p: Path(p).resolve(), required=True,
                        help="Debian ext4 fixture with smolvm-agent and the measured helper installed")
    parser.add_argument("--previous-platformd", type=lambda p: Path(p).resolve(strict=True),
                        help="previous release used to create a genuine historical snapshot")
    cases = ("relay", "fork", "statepoint", "hibernate", "successive-captures")
    parser.add_argument("--case", choices=(*cases, "packet-native", "packet-fixed", "packet-cut-native", "packet-cut-fixed", "packet-activity",
                        "policy", "protected-fork", "grant-rollback", "connection-recovery", "cpu-limit", "cpu-limit-historical",
                        "live-resources", "live-resources-hot", "gate-without-agent", "task-adoption", "task-adoption-delete",
                        "task-restore", "task-wake", "fork-cancel", "fork-lost-reply", "task-fork-cancel-retry", "saved-wake-failure", "saved-wake-exact", "polish-files", "polish-capture", "polish-lease-fork", "polish-metrics", "polish-history", "polish-old-agent", "polish-metrics-epoch"), action="append")
    parser.add_argument("--cc", default="cc", help="Linux compiler and static C runtime for the probe")
    parser.add_argument("--probe", type=lambda p: Path(p).resolve(), help="prebuilt tests/fixtures/vsock-probe.c")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM, a live systemd manager, and Linux socket peer pidfds")
    selection = args.case or cases
    if "polish-history" in selection and args.previous_platformd is None:
        parser.error("polish-history requires --previous-platformd")
    if len(selection) > 1:
        for case in selection:
            command = [sys.executable, __file__, "--bundle", args.bundle, "--platformd", args.platformd,
                       "--system-image", args.system_image, "--cc", args.cc, "--work-dir", args.work_dir,
                       "--case", case]
            if args.previous_platformd:
                command.extend(["--previous-platformd", args.previous_platformd])
            if args.probe:
                command.extend(["--probe", args.probe])
            env = {**os.environ, "MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE": "0"}
            subprocess.run(command, env=env, check=True)
        return
    if os.environ.get("MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "MANAGED_SYSTEM_VSOCK_TEST_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="msv-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        if selection[0].startswith("packet-cut-") or selection[0] == "packet-activity" or selection[0] in INSTALLED_CASES:
            slice_name = f"smolvmcut{os.getpid()}_{os.urandom(4).hex()}.slice"
            subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                            "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                            slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                           check=True, stdout=subprocess.DEVNULL)
            stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=False)
            os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        try:
            test(args, root, stack, selection[0])
        except BaseException:
            for log in [root / "platform.log", root / "worker.log", root / "serve.log",
                        *(root / "state/managed").glob("*.log")]:
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-4500:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
