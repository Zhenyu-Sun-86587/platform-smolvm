#!/usr/bin/env python3
"""Fence a live public file download across a real System execution replacement."""

import argparse
from contextlib import closing, ExitStack
import hashlib
from http.client import IncompleteRead
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
from urllib.parse import urlencode

from managed_system_vsock import Platform, wait_for


SIZE = 96 << 20
TARGET = "/root/polish-file-fence.bin"
NAMESPACE_ORIGIN = "POLISH_FILE_FENCE_PARENT_NAMESPACES"


def authenticated_get(platform, client, route):
    client.request("GET", "/v1/" + route, headers={
        "Origin": platform.origin,
        "Cookie": platform.cookie,
    })
    return client.getresponse()


def constrain_receive_window(platform, client):
    # Set the receive buffer before negotiation so the old stream reaches
    # backpressure even on a fast local TCP connection.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as transport:
        transport.settimeout(client.timeout)
        transport.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 16 << 10)
        transport.connect(("127.0.0.1", platform.port))
        client.sock = transport.dup()


def check_file_fence(platform, path, before, source):
    platform.wait_capability(path, "files.read")
    original = ("original-" + os.urandom(16).hex()).encode()
    replacement = ("replacement-" + os.urandom(16).hex()).encode()
    platform.guest(source, [
        "/bin/sh", "-ec", 'printf %s "$2" >"$1"; truncate -s "$3" "$1"; sync "$1"',
        "old-download", TARGET, original.decode(), str(SIZE),
    ])

    route = path + "/files?" + urlencode({"path": TARGET})
    consumed = 0
    prefix_digest = hashlib.sha256()

    def verify_original(chunk):
        nonlocal consumed
        overlap = original[consumed:consumed + len(chunk)]
        assert chunk.startswith(overlap), f"original download prefix changed at byte {consumed}"
        tail = chunk[len(overlap):]
        assert tail.count(0) == len(tail), f"non-original bytes in old stream at byte {consumed}"
        consumed += len(chunk)
        prefix_digest.update(chunk)

    with closing(platform.api_connection(timeout=45)) as old_connection:
        constrain_receive_window(platform, old_connection)
        with authenticated_get(platform, old_connection, route) as old:
            assert old.status == 200 and old.getheader("Content-Type") == "application/octet-stream", old.status
            assert old.chunked, "expected a streamed response with explicit HTTP chunk termination"
            first = old.read(4096)
            assert len(first) == 4096, len(first)
            verify_original(first)
            held = platform.app(path + "/metrics")["platform"]["active_access_holds"]
            assert held > 0, "file request released its execution hold before the cut"

            # This is the user-visible, Worker-owned stop. The download remains
            # open and unread while the only fixture Guest is stopped and replaced.
            stopped = platform.complete(platform.change(
                path + "/stop", "file-fence-stop", {},
                platform.wait_capability(path, "lifecycle.stop"),
            ))
            assert stopped["source_disposition"] == "EXITED", stopped
            stopped_view = platform.app(path)
            assert stopped_view["power_state"] == "stopped", stopped_view
            started = platform.complete(platform.change(
                path + "/start", "file-fence-start", {}, stopped_view,
            ))
            after, successor = platform.context(before["machine_id"])
            assert successor["instance_id"] == source["instance_id"], successor
            assert successor["execution_epoch"] == source["execution_epoch"] + 1, successor
            assert successor["incarnation_id"] != source["incarnation_id"], successor
            assert after["binding"]["binding_id"] == before["binding"]["binding_id"], after
            wait_for(
                lambda: platform.guest(successor, ["/bin/sh", "-ec", 'printf %s "$2" >"$1"; sync "$1"',
                                                    "new-download", TARGET, replacement.decode()]) == b"",
                retry=(OSError, RuntimeError),
            )

            cut = False
            try:
                while chunk := old.read1(64 << 10):
                    verify_original(chunk)
            except IncompleteRead as failure:
                verify_original(failure.partial)
                cut = True
            except ConnectionResetError:
                cut = True
            assert cut and 4096 <= consumed < SIZE, (
                "old HTTP stream must be explicitly incomplete after replacing its execution", cut, consumed,
            )

    with closing(platform.api_connection(timeout=45)) as new_connection:
        with authenticated_get(platform, new_connection, route) as new:
            assert new.status == 200 and new.getheader("Content-Type") == "application/octet-stream", new.status
            assert new.read(len(replacement) + 1) == replacement, "fresh download did not use the replacement execution"

    wait_for(lambda: platform.app(path + "/metrics")["platform"]["active_access_holds"] == 0)
    print(json.dumps({
        "case": "J02-file-execution-fence",
        "bundle": platform.bundle_id,
        "machine_id": before["machine_id"],
        "binding_id": before["binding"]["binding_id"],
        "source": source,
        "successor": successor,
        "stop_operation_id": stopped["operation_id"],
        "start_operation_id": started["operation_id"],
        "original_size": SIZE,
        "original_stream": "interrupted",
        "original_bytes_verified": consumed,
        "original_prefix_sha256": prefix_digest.hexdigest(),
        "replacement_sha256": hashlib.sha256(replacement).hexdigest(),
    }), flush=True)


def isolated_namespaces(parser):
    current = ":".join(str(Path("/proc/self/ns", name).stat().st_ino) for name in ("mnt", "net"))
    if (origin := os.environ.get(NAMESPACE_ORIGIN)) is None:
        os.execvpe("unshare", [
            "unshare", "--mount", "--propagation", "private", "--net", "--",
            sys.executable, str(Path(__file__).resolve()), *sys.argv[1:],
        ], {**os.environ, NAMESPACE_ORIGIN: current})
    if len(origin.split(":")) != 2 or any(a == b for a, b in zip(origin.split(":"), current.split(":"))):
        parser.error("fixture requires its own mount and network namespaces")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for argument in ("bundle", "platformd", "system-image", "work-dir"):
        parser.add_argument("--" + argument, type=lambda path: Path(path).resolve(), required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires Linux root and KVM")
    if not Path("/run/smolvm-custom").is_dir():
        parser.error("requires the pre-existing fixture bind-mount target /run/smolvm-custom")
    isolated_namespaces(parser)
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="pff-") as scratch, ExitStack() as stack:
        root = Path(scratch)
        root.chmod(0o755)
        unit = f"smolvmfilefence{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run([
            "busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
            "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
            unit, "fail", "1", "MemoryAccounting", "b", "true", "0",
        ], check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=False)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit

        try:
            platform = Platform(args, root, stack, use_installation=True)
            platform.start()
            accepted = platform.change("machines", "file-fence-create", {
                "name": "System file execution fence",
                "template_id": "t_system", "boot_mode": "system",
                "runtime_instance_id": platform.serve_id, "start": True,
                "resources": {"cpu_count": 1, "memory_mib": 512},
            })
            platform.complete(accepted)
            machine = accepted["machine_id"]
            before, source = platform.context(machine)
            check_file_fence(platform, "machines/" + machine, before, source)
        except BaseException:
            for log in (root / "platform.log", root / "worker.log", root / "serve.log"):
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-4000:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
