#!/usr/bin/env python3
"""Exercise managed System vsock on Linux/KVM through installed platform and Worker binaries."""

import argparse
import array
import base64
from contextlib import ExitStack, suppress
from datetime import datetime, timedelta, timezone
import grp
import hashlib
from http.client import HTTPConnection, HTTPResponse
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
import time


EXT = "/api/v1/runtime-ext/v1"


class UnixHTTPConnection(HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
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
    def __init__(self, args, root, stack):
        self.args = args
        self.root = root
        self.stack = stack
        self.worker = root / "ipc/worker.sock"
        self.serve = root / "api/serve.sock"
        self.relay_dir = root / "guest-relay"
        self.cookie = ""
        self.csrf = ""
        self.epoch = ""

    def start(self):
        root = self.root
        for name in ("var-lib", "ipc", "runtime"):
            (root / name).mkdir(mode=0o755)
        subprocess.run(["mount", "--bind", root / "var-lib", "/var/lib"], check=True)
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
        runtime = Path("/run/smolvm-custom")
        runtime.mkdir(exist_ok=True)
        subprocess.run(["mount", "--bind", root / "runtime", runtime], check=True)
        self.bundle = shutil.copytree(self.args.bundle, root / "bundle", symlinks=True)
        self.env = {
            **os.environ,
            "LD_LIBRARY_PATH": str(self.bundle / "lib"),
            "SMOLVM_LIB_DIR": str(self.bundle / "lib"),
            "SMOLVM_GUEST_RELAY_DIR": str(self.relay_dir),
        }
        password = os.urandom(24).hex()
        secret = root / "owner-password"
        secret.touch(mode=0o600)
        secret.write_text(password + "\n")
        self.env["SANDBOX_OWNER_PASSWORD_FILE"] = str(secret)
        self.spawn([self.bundle / "smolvm-resourced", "serve", "--socket", self.worker,
                    "--state-dir", root / "catalog", "--socket-gid", str(grp.getgrnam("kvm").gr_gid)],
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
            spec=dict(boot_mode="system", root_generation_id="fixture-system-root"),
        )]))
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        self.origin = f"http://127.0.0.1:{self.port}"
        config = root / "platform.json"
        config.write_text(json.dumps(dict(
            version=1, state_dir=str(root / "platform"), templates_file=str(templates),
            api=dict(listen=f"127.0.0.1:{self.port}", public_origin=self.origin),
            runtime=dict(socket=str(root / "ipc/official.sock"), status_socket="",
                         data_dir=str(root / "official"), custom=[dict(
                             instance_id=self.serve_id, bundle_digest=self.bundle_id,
                             state_reader_id=handshake["state_reader_id"], socket=str(self.serve),
                             manifest=str(self.bundle / "runtime-bundle.json"),
                             worker_socket=str(self.worker), guest_relay_dir=str(self.relay_dir),
                         )]),
        )))
        self.platform_command = [self.args.platformd, "--config", config]
        self.platform = self.spawn(self.platform_command, "platform.log")
        session, cookie = wait_for(lambda: request(
            HTTPConnection("127.0.0.1", self.port, timeout=15), "POST", "/v1/auth/login",
            dict(username="owner", password=password), {"Origin": self.origin},
        ), retry=(OSError,))
        self.cookie = cookie.split(";", 1)[0]
        self.csrf = session["csrf_token"]
        secret.unlink()
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

    def app(self, path, method="GET", body=None, headers=None, status=200):
        return request(HTTPConnection("127.0.0.1", self.port, timeout=15), method, "/v1/" + path,
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

    def guest(self, ref, command, stdin=None, expected=0):
        command = dict(command=command, timeout_seconds=35)
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


def test(args, root, stack, case):
    platform = Platform(args, root, stack)
    platform.start()
    created = platform.change("machines", "create", dict(
        name="System vsock fixture", template_id="t_system", boot_mode="system",
        runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=2, memory_mib=768),
    ))
    platform.complete(created)
    machine_id = created["machine_id"]
    source = "machines/" + machine_id
    _, ref = platform.context(machine_id)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    assert any(device["virtio_type"] == 19 and device.get("parked_vsock_rebind")
               for device in inventory["inventory"]["devices"])
    binary = args.probe
    if binary is None:
        binary = root / "vsock-probe"
        source_file = Path(__file__).resolve().parents[1] / "fixtures/vsock-probe.c"
        subprocess.run([args.cc, "-static", "-Os", "-s", "-o", binary, source_file], check=True)
    probe = platform.place_probe(ref, binary)
    seen = {int(platform.guest(ref, [probe, "--cid"]))}
    platform.guest(ref, ["/bin/sh", "-c", "printf 'held-source\\n' > /var/tmp/platform-cut-marker; sync"])
    if case != "relay":
        helper = platform.guest(ref, ["/usr/bin/sha256sum", "/sbin/smolvm-identity-helper"]).split()[0]
        assert helper.decode() == digest(platform.bundle / "guest/identity-helper").split(":")[1]
    if case == "relay":
        relay_case(platform, source, machine_id, ref, probe)
    elif case == "fork":
        fork_case(platform, source, ref, probe, seen)
    elif case == "statepoint":
        statepoint_case(platform, source, ref, probe, seen)
    else:
        hibernate_case(platform, source, ref, probe, seen)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=lambda p: Path(p).resolve(), required=True)
    parser.add_argument("--platformd", type=lambda p: Path(p).resolve(), required=True)
    parser.add_argument("--system-image", type=lambda p: Path(p).resolve(), required=True,
                        help="Debian ext4 fixture with smolvm-agent and the measured helper installed")
    cases = ("relay", "fork", "statepoint", "hibernate")
    parser.add_argument("--case", choices=cases, action="append")
    parser.add_argument("--cc", default="cc", help="Linux compiler and static C runtime for the probe")
    parser.add_argument("--probe", type=lambda p: Path(p).resolve(), help="prebuilt tests/fixtures/vsock-probe.c")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM, a live systemd manager, and Linux socket peer pidfds")
    selection = args.case or cases
    if len(selection) > 1:
        for case in selection:
            command = [sys.executable, __file__, "--bundle", args.bundle, "--platformd", args.platformd,
                       "--system-image", args.system_image, "--cc", args.cc, "--work-dir", args.work_dir,
                       "--case", case]
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
