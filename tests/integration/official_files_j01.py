#!/usr/bin/env python3
"""Verify official smolvm 1.37.1 file REST calls on an existing OCI workspace."""

import argparse
from contextlib import ExitStack, closing, suppress
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
from urllib.parse import urlencode

from managed_system_vsock import Platform, UnixHTTPConnection, digest, request, wait_for


VERSION = "1.37.1"


def stop_remaining_stock_guests(executable):
    for process in Path("/proc").glob("[0-9]*"):
        with suppress(OSError):
            if (process / "exe").readlink() != executable:
                continue
            fd = os.pidfd_open(int(process.name))
            try:
                if (process / "exe").readlink() == executable:
                    signal.pidfd_send_signal(fd, signal.SIGKILL)
            finally:
                os.close(fd)


def file_request(platform, method, path, *, body=None, headers=None, status=200):
    with closing(platform.api_connection(timeout=45)) as connection:
        connection.request(method, "/v1/" + path, body, {
            "Origin": platform.origin, "Cookie": platform.cookie,
            "X-CSRF-Token": platform.csrf, **(headers or {}),
        })
        reply = connection.getresponse()
        payload = reply.read()
        if reply.status != status:
            raise AssertionError((method, path, reply.status, payload[:1024]))
        return reply.headers.get_content_type(), payload


def upload(platform, endpoint, data, headers):
    content_type, raw = file_request(platform, "PUT", endpoint, body=data, headers=headers)
    assert content_type == "application/json", content_type
    receipt = json.loads(raw)
    assert receipt["state"] == "uploaded" and receipt["upload_id"], receipt
    assert receipt["size"] == len(data) and receipt["sha256"] == hashlib.sha256(data).hexdigest(), receipt
    return receipt


def check_existing_file(platform, source, endpoint, name, expected):
    content_type, actual = file_request(platform, "GET", endpoint)
    assert content_type == "application/octet-stream" and actual == expected, (content_type, actual)
    entries = platform.app(source + "/files/list")["items"]
    assert {"name": name, "type": "file", "size": len(expected)} in entries, entries


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platformd", type=Path, required=True, help="candidate Platform binary")
    parser.add_argument("--seed-platformd", type=Path, help="previous Platform binary used to create the workspace")
    parser.add_argument("--runtime-archive", type=Path, required=True, help=f"pinned official smolvm {VERSION} archive")
    parser.add_argument("--image", type=Path, required=True, help="offline Debian managed OCI archive")
    args = parser.parse_args()
    args.platformd = args.platformd.resolve(strict=True)
    seed = (args.seed_platformd or args.platformd).resolve(strict=True)
    image = args.image.resolve(strict=True)
    runtime_archive = args.runtime_archive.resolve(strict=True)
    for namespace in ("mnt", "net"):
        if Path(f"/proc/self/ns/{namespace}").stat().st_ino == Path(f"/proc/1/ns/{namespace}").stat().st_ino:
            parser.error("run in private mount and network namespaces using unshare --mount --net --propagation private")
    pin = json.loads((Path(__file__).resolve().parents[2] / "packaging/runtime-manifest.json").read_text())
    archive_digest = digest(runtime_archive)
    assert pin["version"] == VERSION and archive_digest == "sha256:" + pin["sha256"], archive_digest
    image_digest = digest(image)

    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="j01-official-") as directory, ExitStack() as stack:
        root = Path(directory)
        root.chmod(0o755)
        for name in ("var-lib", "ipc", "platform", "official", "stock"):
            (root / name).mkdir()
        subprocess.run(["mount", "--make-rprivate", "/"], check=True)
        subprocess.run(["mount", "--bind", root / "var-lib", "/var/lib"], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", "/var/lib"], check=True)
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
        subprocess.run(["tar", "-xf", runtime_archive, "-C", root / "stock"], check=True)
        runtime = root / "stock" / f"smolvm-{VERSION}-linux-x86_64"
        stack.callback(stop_remaining_stock_guests, runtime / "smolvm-bin")

        password = os.urandom(24).hex()
        secret = root / "owner"
        secret.touch(mode=0o600)
        secret.write_text(password + "\n")
        platform = Platform(args, root, stack)
        platform.env = {k: v for k, v in os.environ.items() if not k.startswith("SMOLVM_")}
        platform.env["SANDBOX_OWNER_PASSWORD_FILE"] = str(secret)
        serve = root / "ipc" / "official.sock"
        runtime_process = platform.spawn([
            runtime / "smolvm", "--state-dir", root / "official", "serve", "start",
            "--listen", f"unix://{serve}", "--local-image-dir", image.parent,
            "--guest-rollout-port", "37567", "--shutdown-grace", "0",
        ], "official.log")
        try:
            health = wait_for(lambda: request(UnixHTTPConnection(serve), "GET", "/health")[0])
            assert health["version"] == VERSION, health
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                platform.port = reservation.getsockname()[1]
            platform.origin = f"http://127.0.0.1:{platform.port}"
            templates = root / "templates.json"
            templates.write_text(json.dumps([{
                "template_id": "t_j01", "name": "Debian Managed OCI", "version": 1,
                "image_digest": image_digest, "guest_release": "v1", "spec": {"local_image": image.name},
            }]))
            config = root / "platform.json"
            config.write_text(json.dumps({
                "version": 1, "state_dir": str(root / "platform"), "templates_file": str(templates),
                "api": {"listen": f"127.0.0.1:{platform.port}", "public_origin": platform.origin},
                "runtime": {"socket": str(serve), "status_socket": "", "data_dir": str(root / "official"),
                            "local_image_dir": str(image.parent)},
            }))
            seed_process = platform.spawn([seed, "--config", config], "seed.log")
            platform.login_owner(password)
            seed_ctl = platform.app("control")
            assert seed_ctl["runtime_version"] == VERSION and seed_ctl["runtime_ready"], seed_ctl
            accepted = platform.change("machines", "j01-create", {
                "name": "Official file compatibility", "template_id": "t_j01", "boot_mode": "oci", "start": True,
                "resources": {"cpu_count": 1, "memory_mib": 384, "storage_gib": 2, "overlay_gib": 2},
                "idle_policy": {"enabled": True, "timeout_seconds": 3600},
            })
            platform.complete(accepted)
            source = "machines/" + accepted["machine_id"]
            before = platform.app(source)
            assert before["boot_mode"] == "oci" and not before["binding"].get("runtime_bundle_id"), before
            assert before["idle_policy"] == {"enabled": True, "timeout_seconds": 3600}, before["idle_policy"]
            assert before["power_state"] == "running", before
            name = "j01-stock.bin"
            path = "/workspace/" + name
            endpoint = source + "/files?" + urlencode({"path": path})
            payload = b"\x00\xfffirst\n"
            put_headers = {
                "Content-Type": "application/octet-stream", "If-Control-Epoch": platform.epoch,
                "Idempotency-Key": "j01-file-original", "If-Match": f'"{before["revision"]}"',
            }
            original = upload(platform, endpoint, payload, put_headers)
            assert original["path"] == path, original

            seed_process.terminate()
            seed_process.wait(timeout=25)
            platform.spawn([args.platformd, "--config", config], "candidate.log")
            platform.login_owner(password)
            candidate_ctl = platform.app("control")
            assert candidate_ctl["runtime_version"] == VERSION and candidate_ctl["runtime_ready"], candidate_ctl
            assert runtime_process.poll() is None and platform.epoch == put_headers["If-Control-Epoch"]
            current = platform.app(source)
            for key in ("machine_id", "template_id", "binding_generation", "runtime_name", "idle_policy"):
                assert current[key] == before[key], key
            assert current["binding"]["binding_id"] == before["binding"]["binding_id"]
            assert current["power_state"] == "running", current
            check_existing_file(platform, source, endpoint, name, payload)
            assert upload(platform, endpoint, payload, put_headers) == original
            updated = b"\x00\xffreplacement\x00\x80"
            replacement = upload(platform, endpoint, updated, {
                **put_headers, "Idempotency-Key": "j01-file-replacement", "If-Match": f'"{current["revision"]}"',
            })
            check_existing_file(platform, source, endpoint, name, updated)
            assert replacement["upload_id"] != original["upload_id"] and replacement["path"] == path
            assert upload(platform, endpoint, payload, put_headers) == original
            check_existing_file(platform, source, endpoint, name, updated)
            platform.complete(platform.change(source, "j01-delete", {}, platform.app(source), method="DELETE"))
            print(json.dumps({
                "case": "j01_official_files", "official_runtime": health["version"],
                "runtime_archive": archive_digest, "image": image_digest,
                "seed_build": seed_ctl["build"], "seed_binary": digest(seed),
                "candidate_build": candidate_ctl["build"], "candidate_binary": digest(args.platformd),
                "file_replay_retained": True, "binary_round_trip": True, "binding_and_idle_policy_retained": True,
            }), flush=True)
        except BaseException:
            for name in ("official.log", "seed.log", "candidate.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
