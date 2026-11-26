#!/usr/bin/env python3
"""Kill a real scheduled backup at a filesystem boundary on a private backup mount."""

import argparse
from contextlib import closing
import ctypes
import errno
import hashlib
import json
import os
from pathlib import Path
import secrets
import select
import signal
import sqlite3
import struct
import subprocess
import sys
import tempfile
import time
from urllib.error import URLError
from urllib.request import Request, urlopen

from registry_migration_crash import admin, configure


def until(observe, seconds=15):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = observe()
        if value:
            return value
        time.sleep(0.04)
    raise TimeoutError("backup did not reach the expected state")


def rows(database):
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        return dict(db.execute("SELECT id,state FROM backup_runs WHERE policy_id='daily'"))


class BackupFiles:
    """Allow opens on this private mount until the selected daemon reaches the requested boundary."""

    header = struct.Struct("=IBBHQii")

    def __init__(self, directory):
        libc = ctypes.CDLL(None, use_errno=True)
        self.fd = libc.fanotify_init(0x04 | 0x01, os.O_RDONLY | os.O_CLOEXEC)  # FAN_CLASS_CONTENT | FAN_CLOEXEC
        if self.fd < 0:
            raise OSError(ctypes.get_errno(), "fanotify_init")
        # FAN_MARK_ADD | FAN_MARK_MOUNT, FAN_OPEN_PERM | FAN_ONDIR
        if libc.fanotify_mark(self.fd, 0x11, ctypes.c_uint64(0x10000 | 0x40000000),
                              -100, os.fsencode(directory)) < 0:
            error = ctypes.get_errno()
            os.close(self.fd)
            raise OSError(error, "fanotify_mark")

    def close(self):
        os.close(self.fd)

    def at_open(self, proc, matches, kill=False, seconds=25):
        deadline = time.monotonic() + seconds
        with closing(os.fdopen(os.pidfd_open(proc.pid), "rb", buffering=0)) as target:
            while time.monotonic() < deadline and proc.poll() is None:
                if not select.select([self.fd], [], [], 0.3)[0]:
                    continue
                batch = os.read(self.fd, 8192)
                cursor = 0
                found = False
                while cursor < len(batch):
                    size, version, _, _, _, fd, pid = self.header.unpack_from(batch, cursor)
                    assert version == 3 and fd >= 0 and size >= self.header.size
                    cursor += size
                    try:
                        path = Path(os.readlink(f"/proc/self/fd/{fd}"))
                        selected = not found and pid == proc.pid and matches(path, os.fstat(fd))
                        found = found or selected
                        if selected and kill:
                            signal.pidfd_send_signal(target.fileno(), signal.SIGKILL)
                        try:
                            os.write(self.fd, struct.pack("=iI", fd, 1))  # FAN_ALLOW
                        except OSError as exc:
                            if exc.errno != errno.ENOENT or not (kill and found):
                                raise
                    finally:
                        os.close(fd)
                if found:
                    if kill:
                        assert proc.wait(timeout=8) == -signal.SIGKILL
                    return
        raise TimeoutError("the daemon did not open the selected backup file")


class Installation:
    def __init__(self, args, root):
        self.args = args
        self.root = root
        self.database = root / "state/platform.db"
        self.directory = root / "archives"
        self.password = secrets.token_urlsafe()
        self.secret = root / "password"
        key = root / "recovery-key"
        for path, value in ((self.secret, self.password.encode()), (key, os.urandom(32))):
            with open(path, "xb", opener=lambda p, flags: os.open(p, flags, 0o600)) as handle:
                handle.write(value)
        self.config, self.origin = configure(root)
        cfg = json.loads(self.config.read_text())
        cfg["backups"] = [{"id": "daily", "scope": "platform_snapshots", "interval_seconds": 86400,
                           "retain": 1, "directory": str(self.directory), "recovery_key_file": str(key)}]
        self.config.write_text(json.dumps(cfg))
        self.process = None
        self.log = (root / "platform.log").open("ab")

    def start(self):
        self.process = subprocess.Popen([self.args.platformd, "--config", self.config],
                                       env={**os.environ, "SANDBOX_OWNER_PASSWORD_FILE": str(self.secret)},
                                       stdout=self.log, stderr=self.log)
        login = Request(self.origin + "/v1/auth/login", json.dumps({"username": "owner",
                        "password": self.password}).encode(), {"Content-Type": "application/json"})

        def connected():
            if self.process.poll() is not None:
                raise RuntimeError(f"platformd exited {self.process.returncode}")
            try:
                with urlopen(login, timeout=2) as reply:
                    self.cookie = reply.headers["Set-Cookie"].partition(";")[0]
                    self.csrf = json.load(reply)["csrf_token"]
                    return True
            except URLError as exc:
                if not isinstance(exc.reason, ConnectionRefusedError):
                    raise
                return False

        until(connected)

    def create(self):
        with closing(sqlite3.connect(self.database.as_uri() + "?mode=ro", uri=True)) as db:
            epoch, = db.execute("SELECT control_epoch FROM meta").fetchone()
        request = Request(self.origin + "/v1/backups/policies/daily/runs", b"{}", {
            "Content-Type": "application/json", "Origin": self.origin, "Cookie": self.cookie,
            "X-CSRF-Token": self.csrf, "If-Control-Epoch": epoch, "Idempotency-Key": "second-run",
        })
        with urlopen(request, timeout=5) as response:
            assert response.status == 202
            return json.load(response)["backup_id"]

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
    def close(self):
        self.stop()
        self.log.close()


def exercise(args, root):
    app = Installation(args, root)
    app.directory.mkdir()
    subprocess.run(["mount", "--bind", app.directory, app.directory], check=True)
    try:
        app.start()
        def baseline():
            return next((key for key, state in rows(app.database).items() if state == "verified"), None)
        first = until(baseline)
        previous = app.directory / first
        good = admin(args.cli, app.config, "backup", "verify", "--archive", previous)
        with closing(BackupFiles(app.directory)) as events:
            new = app.create()
            stage = app.directory / (".sandbox-" + new)
            target = app.directory / new
            marker = stage.with_name(stage.name + ".catalog-ready")

            def at_boundary(path, meta):
                if args.case == "staging":
                    return path == stage / "backup-manifest.json" and meta.st_size > 0
                if args.case == "published":
                    return path == app.directory and target.is_dir() and rows(app.database).get(new) == "running"
                if args.case in ("retiring", "unlinked"):
                    if rows(app.database).get(first) != "retiring":
                        return False
                    if args.case == "retiring":
                        return previous.is_dir()
                    return path == app.directory and not previous.exists()
                return path == previous / "backup-manifest.json" and rows(app.database).get(new) == "verified"

            events.at_open(app.process, at_boundary, kill=True)
        if args.case != "unlinked":
            assert admin(args.cli, app.config, "backup", "verify", "--archive", previous) == good
        state = rows(app.database)
        expected_prior = "retiring" if args.case in ("retiring", "unlinked") else "verified"
        assert len(state) == 2 and state[first] == expected_prior, state
        if args.case == "staging":
            assert stage.is_dir() and marker.is_file() and not target.exists() and state[new] == "running"
        else:
            assert target.is_dir() and not stage.exists()
            assert state[new] == ("running" if args.case == "published" else "verified")
        pinned_copy = (stage if args.case == "staging" else target) / "platform.db"
        identity = pinned_copy.stat()
        checksum = hashlib.sha256(pinned_copy.read_bytes()).hexdigest()
        print(f"{args.case}: platformd killed at the filesystem boundary, prior receipt={state[first]}, "
              f"new receipt={state[new]}", flush=True)

        if args.case == "corrupt-kept":
            member = target / "recovery.sealed"
            original = member.read_bytes()
            member.write_bytes(original[:-1] + bytes([original[-1] ^ 32]))
            with closing(BackupFiles(app.directory)) as events:
                app.start()
                events.at_open(app.process, lambda path, _: path == previous / "backup-manifest.json")
            assert admin(args.cli, app.config, "backup", "verify", "--archive", previous) == good
            assert rows(app.database)[first] == "verified"
            app.stop()
            member.write_bytes(original)
            print("corrupt-kept: the scheduler checked the retained archives and preserved the previous "
                  "authenticated copy while the newest one was damaged", flush=True)
        app.start()
        until(lambda: rows(app.database) == {first: "retired", new: "verified"})
        assert app.create() == new and len(rows(app.database)) == 2
        assert not previous.exists() and not stage.exists() and not marker.exists(), sorted(app.directory.iterdir())
        assert os.path.samestat(identity, (target / "platform.db").stat())
        assert hashlib.sha256((target / "platform.db").read_bytes()).hexdigest() == checksum
        committed = admin(args.cli, app.config, "backup", "verify", "--archive", target)
        assert committed["backup_id"] == new and committed["installation_id"] == good["installation_id"]
        print(f"{args.case}: same public run and same database inode recovered; new archive authenticated "
              "and prior retention completed before the daily schedule", flush=True)
    finally:
        app.close()
        subprocess.run(["umount", app.directory], check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("platformd", "cli"):
        parser.add_argument("--" + flag, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--case", choices=["staging", "published", "retention", "retiring", "unlinked",
                                          "corrupt-kept"], required=True)
    parser.add_argument("--work-dir", type=Path)
    parser.add_argument("--private-mount", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("this integration fixture requires a private mount namespace and fanotify")
    if not args.private_mount:
        os.execvp("unshare", ["unshare", "--mount", "--propagation", "private", "--",
                              sys.executable, *sys.argv, "--private-mount"])
    with tempfile.TemporaryDirectory(prefix="backup-crash-", dir=args.work_dir) as folder:
        exercise(args, Path(folder).resolve())
