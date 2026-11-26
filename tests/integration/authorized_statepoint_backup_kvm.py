#!/usr/bin/env python3
"""Create, verify and recover a public native backup on an installed Bundle."""

import argparse
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import secrets
import select
import signal
import subprocess
import sys
import tempfile

from authorized_services_kvm import identity, witness
from managed_system_vsock import digest, wait_for, worker
from native_backup_restart_kvm import BackupPlatform, db_rows


MARKERS = ("/run/a19-backup-ram", "/var/tmp/a19-backup-disk")


def cli(platform, *arguments):
    result = subprocess.run([platform.args.sandboxctl, "--json", *arguments],
                            env={**platform.env, "SANDBOX_TOKEN": platform.cli_token, "SANDBOX_URL": platform.origin},
                            text=True, capture_output=True, timeout=190)
    assert result.returncode == 0, (result.stdout, result.stderr)
    return json.loads(result.stdout)


def reopen(platform, archive, native, point, marker):
    destination = platform.root / "independent"
    assert not destination.exists()
    platform.backup_admin("recover-native", "--archive", archive, "--output", destination, "--worker-catalog")
    catalog = destination / Path(native["directory"]).name
    db = catalog / "catalog.sqlite"
    assert db_rows(db, "SELECT manifest_digest FROM statepoints WHERE capture_id=?", point["capture_id"]) == [
        (point["manifest_digest"],)
    ]
    assert not db_rows(db, "SELECT 1 FROM executions WHERE phase!='exited'")
    ipc = platform.root / "recovery-ipc"
    ipc.mkdir(mode=0o755)
    worker_socket = ipc / "worker.sock"
    serve_socket = platform.root / "recovery-api/serve.sock"
    platform.start_worker(worker_socket, catalog, original=False)
    health = wait_for(lambda: worker(worker_socket, "Health"), retry=(OSError, RuntimeError))
    if health["state"] == "recovering":
        worker(worker_socket, "CompleteRecovery", {"broker_session_id": health["broker_session_id"]})
        health = worker(worker_socket, "Health")
    assert health["state"] == "ready" and health["executable_sha256"] == digest(platform.bundle / "smolvm-resourced"), health
    platform.spawn([platform.bundle / "smolvm-bin", "--state-dir", platform.root / "recovery-serve",
                    "serve", "start", "--listen", f"unix://{serve_socket}", "--worker-resource-socket", worker_socket,
                    "--guest-rollout-port", "37346", "--shutdown-grace", "0"], "recovery-serve.log")
    original_socket = platform.serve
    platform.serve = serve_socket
    try:
        hello = wait_for(lambda: platform.native("/capabilities"), retry=(OSError, RuntimeError))
        assert hello["bundle_digest"] == platform.bundle_id and hello["state_reader_id"] == native["state_reader_id"], hello
        name = "from-verified-archive"
        unused = identity(platform.native(f"/machines/{name}/binding"))
        operation = "a19-restore-verified-backup"
        platform.native("/operations", {"operation_id": operation, "kind": "vm_restore", "expected": unused,
                        "request_digest": "sha256:" + hashlib.sha256(operation.encode()).hexdigest(),
                        "parameters": {"name": name, "capture_id": point["capture_id"], "mode": "clone_conflicts"}})

        def restored():
            receipt = platform.native("/operations/" + operation)
            assert receipt["phase"] != "ABORTED", receipt
            return receipt if receipt["phase"] == "COMPLETED" else None

        receipt = wait_for(restored, timeout=150)
        assert receipt["statepoint"]["manifest_digest"] == point["manifest_digest"], receipt
        ref = platform.native(f"/machines/{name}/execution")
        assert ref["execution_epoch"] == 1 and ref["incarnation_id"] != unused["incarnation_id"], ref
        execution = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]
        assert identity(execution) == identity(ref), execution
        pid = execution["pid"]
        descriptor = os.pidfd_open(pid)
        try:
            start = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
            assert start == execution["pid_start_time"], execution
            try:
                for path in MARKERS:
                    assert platform.guest(ref, ["cat", path]).decode() == marker
            finally:
                signal.pidfd_send_signal(descriptor, signal.SIGKILL)
                assert select.select([descriptor], [], [], 5)[0], "fixture restoration did not exit"
        finally:
            os.close(descriptor)
        return ref
    finally:
        platform.serve = original_socket


def exercise(platform):
    manifest = json.loads((platform.bundle / "runtime-bundle.json").read_bytes())
    assert platform.bundle_id == manifest["bundle_digest"], manifest["bundle_digest"]
    assets = {entry["path"]: entry["sha256"] for entry in manifest["artifacts"]}
    health = worker(platform.worker, "Health")
    assert health["executable_sha256"] == "sha256:" + assets["smolvm-resourced"], health
    owner = platform.change("tokens", "a19-backup-owner", {"label": "Owned Backup verification",
                            "scope": {"access": "manage"}}, status=200)
    platform.cli_token = owner["token"]
    original = cli(platform, "machines", "create", "Owned archived execution", "--template", "t_system",
                   "--boot-mode", "system", "--runtime-instance", platform.serve_id, "--cpu", "1", "--memory-mib", "512",
                   "--idle=false")["machine_id"]
    _, source = platform.context(original)
    wait_for(lambda: platform.guest(source, ["cat", "/proc/1/comm"]) == b"systemd\n", retry=(OSError, RuntimeError))
    marker = "a19-new-reader-" + secrets.token_hex(12)
    platform.guest(source, ["/bin/sh", "-ec", 'printf %s "$1" > "$2"; printf %s "$1" > "$3"; sync "$3"',
                            "owned", marker, *MARKERS])
    snapshot = cli(platform, "snapshots", "capture", original, "--capture-mode", "vm", "--label", "Owned history")["result"]
    point = snapshot["statepoint"]
    assert point["phase"] == "durable", point
    backup_id = cli(platform, "backups", "run", "native")["backup_id"]
    backup = platform.backup(backup_id)
    archive = platform.root / "archives" / backup_id
    verified = platform.backup_admin("verify", "--archive", archive)
    assert verified["backup_id"] == backup["backup_id"]
    native, = verified["native_closures"]
    assert native["runtime_instance_id"] == platform.serve_id
    assert native["bundle_digest"] == platform.bundle_id and native["state_reader_id"] == manifest["contracts"]["state_reader_id"]
    closure = archive / native["directory"] / "closure.json"
    assert digest(closure) == native["manifest_digest"]
    assert platform.guest(source, ["cat", MARKERS[0]]).decode() == marker
    restored = reopen(platform, archive, native, point, marker)
    assert restored["instance_id"] != source["instance_id"]
    assert identity(platform.context(original)[1]) == identity(source)
    witness(platform, "a19-public-native-backup", source=identity(source), restored=identity(restored),
            capture=point["capture_id"], point_digest=point["manifest_digest"], backup_id=backup_id,
            backup_state=backup["state"], state_reader_id=native["state_reader_id"], closure_digest=native["manifest_digest"], marker=marker,
            platformd=digest(platform.args.platformd), sandboxctl=digest(platform.args.sandboxctl),
            system_image=digest(platform.args.system_image), worker=health["executable_sha256"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "sandboxctl", "system-image"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(strict=True), required=True)
    args = parser.parse_args()
    args.previous_platformd = None
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("A19_PUBLIC_BACKUP_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "A19_PUBLIC_BACKUP_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="a19-public-backup-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=6G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        (root / "archives").mkdir(mode=0o700)
        unit = f"smolvmhistorybackup{os.getpid()}_{secrets.token_hex(4)}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        platform = BackupPlatform(args, root, stack, state_tmpfs=True, use_installation=True)
        try:
            platform.start()
            exercise(platform)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-3500:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
