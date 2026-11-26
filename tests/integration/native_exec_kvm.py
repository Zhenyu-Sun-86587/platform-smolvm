#!/usr/bin/env python3
"""Verify public reconnectable System Exec sessions on Linux/KVM."""

import argparse
import base64
from contextlib import ExitStack, closing, contextmanager
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time

from managed_system_vsock import Platform, wait_for, worker


@contextmanager
def attach(platform, session, cursor, ticket):
    with socket.create_connection(("127.0.0.1", platform.port), timeout=15) as connection:
        connection.sendall((
            f"GET /v1/{session}/attach?cursor={cursor} HTTP/1.1\r\n"
            f"Host: 127.0.0.1:{platform.port}\r\nOrigin: {platform.origin}\r\n"
            "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Key: {base64.b64encode(os.urandom(16)).decode()}\r\n"
            f"Sec-WebSocket-Protocol: sandbox.terminal.v1, ticket.{ticket}\r\n\r\n"
        ).encode())
        with connection.makefile("rb") as stream:
            status = stream.readline()
            assert status.startswith(b"HTTP/1.1 101 "), status
            headers = {}
            while (line := stream.readline()) != b"\r\n":
                key, value = line.split(b":", 1)
                headers[key.lower()] = value.strip()
            assert headers[b"sec-websocket-protocol"] == b"sandbox.terminal.v1", headers

            def receive():
                head = stream.read(2)
                if not head:
                    raise EOFError("terminal connection closed")
                opcode, size = head
                assert opcode in (0x81, 0x82), head
                if size in (126, 127):
                    size = int.from_bytes(stream.read(2 if size == 126 else 8), "big")
                assert 0 < size <= 1 << 20, size
                payload = stream.read(size)
                assert len(payload) == size, (len(payload), size)
                return payload if opcode == 0x82 else json.loads(payload)

            yield receive


def test(args, root, stack):
    network = dict(network_id="exec-net", route_domain_id="fixture-route", cidr="10.64.42.0/24")
    platform = Platform(args, root, stack, (network,), use_installation=args.authorized_only)
    platform.start()
    created = platform.change("machines", "create-exec-system", dict(
        name="Reconnectable exec", template_id="t_system", boot_mode="system",
        runtime_instance_id=platform.serve_id, start=True, resources=dict(cpu_count=1, memory_mib=512),
        network=dict(network_id="exec-net", mode="native"),
        lifecycle_policy=dict(auto_standby=False, auto_hibernate=False, min_running_after_wake_seconds=0),
    ))
    platform.complete(created)
    machine = created["machine_id"]
    source = "machines/" + machine
    _, ref = platform.context(machine)
    wait_for(lambda: platform.guest(ref, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(OSError, RuntimeError))

    # Establish the task manager's exact binding so a listener-only guest
    # reports known zero tasks throughout the idle observation.
    with closing(sqlite3.connect(platform.root / "platform/platform.db")) as db:
        binding, generation, namespace = db.execute("SELECT b.id,b.generation,b.task_namespace FROM machines m "
            "JOIN runtime_bindings b ON b.id=m.current_binding_id WHERE m.id=?", (machine,)).fetchone()
    task_scope = dict(machine_id=machine, generation=generation, task_namespace=namespace, nonce=binding)
    bound = json.loads(platform.guest(ref, ["/usr/local/bin/taskctl", "rpc"], json.dumps(dict(method="bind", binding=task_scope))))
    assert bound["binding"] == task_scope, bound
    platform.guest(ref, ["/usr/bin/systemd-run", "--unit=exec-idle-listener", "--socket-property=ListenStream=127.0.0.1:18399", "/bin/cat"])
    assert platform.guest(ref, ["/usr/bin/systemctl", "is-active", "exec-idle-listener.socket"]) == b"active\n"

    def issue(access):
        return platform.change("tokens", "exec-token-" + access, dict(label="Exec " + access,
                               scope=dict(access=access, machine_id=machine)), status=200)

    manage, readonly = issue("manage"), issue("read")

    def as_user(path, method="GET", body=None, headers=None, status=200, credential=manage):
        return platform.app(path, method, body, {"Cookie": "", "Authorization": "Bearer " + credential["token"],
                                                **(headers or {})}, status, timeout=40)

    def endpoints():
        return {"If-Control-Epoch": platform.epoch, "If-Match": f'"{platform.app(source)["revision"]}"'}

    def start(key, spec):
        return as_user(source + "/execs", "POST", spec, {**endpoints(), "Idempotency-Key": key})

    def protection():
        return worker(platform.worker, "GetLifecycleStatus", route_id="deep", bundle_digest=platform.bundle_id, expected=ref)

    def standby():
        try:
            wait_for(lambda: protection()["power"] == "STANDBY", timeout=35)
        except TimeoutError as error:
            raise AssertionError(protection()) from error

    def ticket(session, credential=manage):
        return as_user(session + "/attach-ticket", "POST", {}, credential=credential)["ticket"]

    def through_exit(receive):
        data = b""
        for _ in range(100):
            item = receive()
            if isinstance(item, bytes):
                data += item
            elif item["type"] == "exit":
                return data, item
            else:
                raise AssertionError(item)
        raise AssertionError("terminal did not report its exit code")

    spec = dict(argv=["/bin/sh", "-ec", 'printf "before:%s:%s:%s\\n" "$RECONNECT_MARKER" "$(pwd)" "$(stty size)"; '
                      'IFS= read -r first; IFS= read -r second; printf "after:%s:%s:%s\\n" "$first" "$second" "$(stty size)"; exit 17'],
                env={"RECONNECT_MARKER": "marker"}, tty=True, interactive=True,
                cols=80, rows=24, timeout_seconds=90)
    headers = {**endpoints(), "Idempotency-Key": "reconnectable-pty"}
    if not args.authorized_only:
        denied = as_user(source + "/execs", "POST", spec, headers, status=403, credential=readonly)
        assert denied["error"]["code"] == "PERMISSION_DENIED", denied
    if args.expect_unsupported:
        old_spec = dict(argv=["/bin/sh", "-ec", "touch /run/old-agent-exec-marker"], tty=True)
        unsupported = as_user(source + "/execs", "POST", old_spec, headers, status=409)
        assert unsupported["error"]["code"] == "NOT_SUPPORTED" and "system-exec-stream-v1" in unsupported["error"]["message"], unsupported
        platform.guest(ref, ["/usr/bin/test", "!", "-e", "/run/old-agent-exec-marker"])
        wait_for(lambda: "hold:api_access" not in protection()["blockers"], timeout=15)
        print("Original frozen System Agent reported NOT_SUPPORTED before command dispatch and released the protection on", platform.bundle_id, flush=True)
        return
    first = as_user(source + "/execs", "POST", spec, headers)
    assert first["state"] == "running" and not first["done"], first
    assert as_user(source + "/execs", "POST", spec, headers)["exec_id"] == first["exec_id"]
    session = "execs/" + first["exec_id"]
    once = ticket(session)
    with attach(platform, session, 0, once) as receive:
        assert receive() == {"type": "ready", "stdin_seq": 0}
        initial = b""
        while b"before:marker:/:24 80\r\n" not in initial:
            initial += receive()
    cursor = len(initial)
    if not args.authorized_only:
        used = as_user(session + "/attach", headers={"Cookie": "", "Authorization": "",
                       "Sec-WebSocket-Protocol": "sandbox.terminal.v1, ticket." + once}, status=401)
        assert used["error"]["code"] == "UNAUTHENTICATED", used
        denied = as_user(session + "/stdin", "POST", {"seq": 1, "eof": True}, status=403, credential=readonly)
        assert denied["error"]["code"] == "PERMISSION_DENIED", denied

    policy = platform.app(source + "/lifecycle")
    platform.complete(platform.change(source, "idle-detached-exec", dict(expected_policy_revision=policy["desired_revision"],
        lifecycle_policy=dict(auto_standby=True, standby_after_seconds=2, auto_hibernate=False, min_running_after_wake_seconds=0)),
        platform.app(source), method="PATCH"))
    time.sleep(5)
    active = protection()
    assert active["power"] == "RUNNING" and "hold:api_access" in active["blockers"], active
    assert active["execution_epoch"] == ref["execution_epoch"] and active["incarnation_id"] == ref["incarnation_id"], active
    print("Detached, silent PTY exceeded the two-second idle threshold with Worker blockers", active["blockers"], flush=True)

    as_user(session + "/resize", "POST", {"cols": 101, "rows": 31})

    def stdin(sequence, text, status=200):
        return as_user(session + "/stdin", "POST", dict(seq=sequence, data_base64=base64.b64encode(text).decode()), status=status)

    assert stdin(1, b"one\n") == {"stdin_seq": 1}
    assert stdin(1, b"one\n") == {"stdin_seq": 1}
    if not args.authorized_only:
        assert stdin(3, b"out-of-order\n", 409)["error"]["code"] == "STDIN_SEQUENCE_CONFLICT"
    with attach(platform, session, cursor, ticket(session)) as receive:
        assert receive() == {"type": "ready", "stdin_seq": 1}
        assert stdin(2, b"two\n") == {"stdin_seq": 2}
        output, exit_frame = through_exit(receive)
        assert b"before:" not in output and b"after:one:two:31 101\r\n" in output, output
        assert exit_frame == {"type": "exit", "exit_code": 17, "state": "exited"}, exit_frame
    final = as_user(session + f"/output?cursor={cursor}")
    assert final["done"] and final["exit_code"] == 17 and final["next"] == cursor + len(output), final
    standby()
    print("The owner obtained a fresh ticket, reattached at the saved cursor, resized the terminal and received exit 17. The listener-only execution then entered Standby.", flush=True)

    immediate = start("non-tty-eof", dict(argv=["/bin/sh", "-ec", "! read unused; printf stdout; printf stderr >&2; exit 9"], wait_seconds=10))
    piped = "execs/" + immediate["exec_id"]
    final = wait_for(lambda: output if (output := as_user(piped + "/output"))["done"] else None)
    assert final["exit_code"] == 9 and {
        (chunk["stream"], base64.b64decode(chunk["data"])) for chunk in final["output"]
    } == {("stdout", b"stdout"), ("stderr", b"stderr")}, final

    interrupted = start("platform-process-death", dict(argv=["/bin/sh", "-ec", 'echo $$ > /run/exec-reconnect.pid; '
        'printf running; exec /bin/sleep 90'], wait_seconds=10))
    old = "execs/" + interrupted["exec_id"]
    assert not interrupted["done"] and interrupted["output"], interrupted
    with attach(platform, old, interrupted["next"], ticket(old, readonly)) as receive:
        assert receive() == {"type": "ready", "stdin_seq": 0}
        platform.app("tokens/" + readonly["token_id"], "DELETE", {}, {"If-Control-Epoch": platform.epoch,
                     "If-Match": '"1"', "Idempotency-Key": "revoke-terminal-reader"})
        try:
            receive()
        except EOFError:
            pass
        else:
            raise AssertionError("revoked terminal reader was still connected")
    platform.restart()
    lost = as_user(old)
    assert lost["state"] == "session_lost" and lost["done"] and "exit_code" not in lost, lost
    if not args.authorized_only:
        assert as_user(old + "/stdin", "POST", {"seq": 1, "eof": True}, status=409)["error"]["code"] == "SESSION_LOST"
    reaper = start("verify-agent-reaped-old-exec", dict(argv=["/bin/sh", "-ec", 'pid=$(cat /run/exec-reconnect.pid); '
        'for i in $(seq 1 30); do test ! -e "/proc/$pid" && exit 0; sleep 0.1; done; exit 1']))
    reaped = wait_for(lambda: row if (row := as_user("execs/" + reaper["exec_id"]))["done"] else None)
    assert reaped["exit_code"] == 0, reaped
    standby()
    print("Piped EOF preserved separated output and exit 9. Revoked reader closed, and platform process death reaped the Guest process, fenced the old session and released the idle hold.", flush=True)
    print("Verified native reconnectable Exec on installed bundle", platform.bundle_id, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(option, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--expect-unsupported", action="store_true", help="verify the original System Agent rejects streaming")
    parser.add_argument("--authorized-only", action="store_true",
                        help="exercise current owner sessions, fresh tickets and authorized operations on an installed Bundle")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, KVM and a live systemd manager")
    if os.environ.get("NATIVE_EXEC_TEST_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "NATIVE_EXEC_TEST_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="exec-kvm-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        (root / "platform").mkdir()
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G", "tmpfs", root / "platform"], check=True)
        stack.callback(subprocess.run, ["umount", root / "platform"], check=True)
        try:
            test(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(f"{name}\n{log.read_text(errors='replace')[-3000:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
