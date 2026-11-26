#!/usr/bin/env python3
"""Interrupt a real release-to-release SQLite migration, then restore its old backup."""

import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import secrets
import select
import shutil
import signal
import socket
import sqlite3
import subprocess
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def db_fingerprint(database):
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        assert db.execute("PRAGMA integrity_check").fetchall() == [("ok",)]
        assert not db.execute("PRAGMA foreign_key_check").fetchall()
        version, = db.execute("PRAGMA user_version").fetchone()
        tables = db.execute("SELECT count(*) FROM sqlite_master WHERE type='table'").fetchone()[0]
        contents = hashlib.sha256("\n".join(db.iterdump()).encode()).hexdigest()
        return version, tables, contents


def configure(root):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    address = f"127.0.0.1:{port}"
    origin = f"http://{address}"
    config = root / "config.yaml"
    config.write_text(json.dumps({
        "version": 1, "state_dir": str(root / "state"),
        "api": {"listen": address, "public_origin": origin},
        "runtime": {"socket": str(root / "official.sock"), "status_socket": "",
                    "data_dir": str(root / "runtime"), "local_image_dir": str(root / "images")},
    }))
    return config, origin


def reject_old_epoch(origin, session, csrf_token, epoch):
    request = Request(origin + "/v1/machines/machine", b'{"name":"stale-mutation","expected_generation":1}',
                      {"Content-Type": "application/json", "Origin": origin, "Cookie": session,
                       "X-CSRF-Token": csrf_token, "If-Control-Epoch": epoch, "If-Match": '"1"',
                       "Idempotency-Key": "pre-restore-change"}, method="PATCH")
    try:
        with urlopen(request, timeout=3) as response:
            raise AssertionError(f"old control epoch accepted, status {response.status}")
    except HTTPError as failure:
        with failure:
            error = json.load(failure)["error"]
        assert failure.code == 409 and error["code"] == "CONTROL_EPOCH_CHANGED" and error["effect"] == "none", error


def serve_once(binary, config, origin, password_file, stale_epoch=None):
    env = {**os.environ, "SANDBOX_OWNER_USER": "owner", "SANDBOX_OWNER_PASSWORD_FILE": str(password_file)}
    body = json.dumps({"username": "owner", "password": password_file.read_text()}).encode()
    request = Request(origin + "/v1/auth/login", body, {"Content-Type": "application/json", "Origin": origin})
    process = subprocess.Popen([binary, "--config", config], env=env, stderr=subprocess.PIPE, text=True)
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError(process.stderr.read())
            try:
                with urlopen(request, timeout=2) as response:
                    login = json.load(response)
                    if stale_epoch:
                        session = response.headers["Set-Cookie"].partition(";")[0]
                        reject_old_epoch(origin, session, login["csrf_token"], stale_epoch)
                    return login["principal"]["principal_id"]
            except URLError as error:
                if not isinstance(error.reason, ConnectionRefusedError):
                    raise
                time.sleep(0.04)
        raise TimeoutError("platformd did not accept a login")
    finally:
        if process.poll() is None:
            process.terminate()
        try:
            log = process.communicate(timeout=10)[1]
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate()
            raise
        if process.returncode:
            raise RuntimeError(f"{binary.name} exited {process.returncode}: {log}")


def add_legacy_pause(database):
    with closing(sqlite3.connect(database)) as db, db:
        db.execute("PRAGMA foreign_keys=ON")
        db.executescript("""
            BEGIN;
            INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json)
              VALUES('old-template','debian',1,'legacy','{}','{}');
            INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,
                                 spec_json,idle_policy_json,keep_running)
              VALUES('machine','legacy-machine','old-template',1,1,'paused','{}','{}',0);
            INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,
                                         spec_hash,observed_state,runtime_pause_id,pause_status)
              VALUES('binding','machine',1,'legacy-machine','current','old-tasks','legacy-spec',
                     'paused','legacy-pause-id','active');
            UPDATE machines SET current_binding_id='binding' WHERE id='machine';
            COMMIT;
        """)
        return db.execute("SELECT installation_id,control_epoch FROM meta WHERE id=1").fetchone()


def interrupt_migration(binary, config, database, preload):
    wal = Path(str(database) + "-wal")
    read_fd, write_fd = os.pipe()
    try:
        env = {**os.environ, "LD_PRELOAD": str(preload), "MIGRATION_TEST_WAL": str(wal),
               "MIGRATION_TEST_PIPE": str(write_fd)}
        proc = subprocess.Popen([binary, "--config", config], pass_fds=(write_fd,),
                                env=env, stderr=subprocess.PIPE, text=True)
    except OSError:
        os.close(read_fd)
        raise
    finally:
        os.close(write_fd)
    try:
        with os.fdopen(read_fd, "rb", buffering=0) as notify:
            if not select.select([notify], [], [], 18)[0] or notify.read(1) != b"W":
                raise RuntimeError(f"migration did not reach the WAL write, exit status {proc.poll()}")
            data = wal.read_bytes()
            page_size = int.from_bytes(data[8:12], "big")
            second = 32 + 24 + page_size
            assert int.from_bytes(data[:4], "big") in (0x377F0682, 0x377F0683)
            assert len(data) >= second + 24 and 512 <= page_size <= 65536
            for frame in (32, second):
                assert int.from_bytes(data[frame:frame+4], "big") > 0
                assert data[frame+4:frame+8] == bytes(4), "frame already has a commit marker"
            return page_size, len(data)
    finally:
        if proc.poll() is None:
            with closing(os.fdopen(os.pidfd_open(proc.pid), "rb", buffering=0)) as handle:
                signal.pidfd_send_signal(handle.fileno(), signal.SIGKILL)
        diagnostic = proc.communicate(timeout=8)[1]
        if proc.returncode != -signal.SIGKILL:
            raise RuntimeError(f"migration exited {proc.returncode}: {diagnostic}")


def admin(cli, config, *args):
    command = [cli, "--json", "admin", "--config", config, *args]
    return json.loads(subprocess.check_output(command, text=True, timeout=50))


def verify_current(database, version, owner, installation, epoch):
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        assert db.execute("PRAGMA user_version").fetchone() == (version,)
        assert db.execute("SELECT schema_version,installation_id,control_epoch FROM meta").fetchone() == (
            version, installation, epoch)
        assert db.execute("SELECT principal_id FROM installation_owner").fetchall() == [(owner,)]
        actual = db.execute("""SELECT b.id,b.generation,b.task_namespace,b.runtime_pause_id,b.pause_status,
                                   b.power_state,b.runtime_bundle_id
                               FROM machines m JOIN runtime_bindings b ON m.current_binding_id=b.id
                               WHERE m.id='machine'""").fetchone()
        assert actual == ("binding", 1, "old-tasks", "legacy-pause-id", "active", "unknown", None), actual
        assert db.execute("PRAGMA foreign_key_check").fetchall() == []
        assert db.execute("PRAGMA integrity_check").fetchall() == [("ok",)]


def recover_old_release(args, root, archive, password, owner, installation, original_epoch, new_epoch):
    target = root / "historical-recovery"
    state = target / "state"
    state.mkdir(parents=True)
    old_db = state / "platform.db"
    config, origin = configure(target)
    # Restore an offline copy of the verified pre-upgrade catalog and same-installation keys.
    shutil.copy2(archive / "platform.db", old_db)
    shutil.copytree(root / "state/secrets", state / "secrets")
    admin(args.old_cli, config, "maintenance", "enter", "--reason", "recover pre-upgrade schema")
    old = admin(args.old_cli, config, "backup", "restore", "--archive", archive,
                "--rollback-output", target / "before")
    control = old["control"]
    assert control["schema_version"] == 2 and control["installation_id"] == installation
    assert control["maintenance"] and control["control_epoch"] not in (new_epoch, original_epoch)
    admin(args.old_cli, config, "maintenance", "exit")
    assert serve_once(args.old_platformd, config, origin, password, original_epoch) == owner
    with closing(sqlite3.connect(old_db.as_uri() + "?mode=ro", uri=True)) as db:
        assert db.execute("PRAGMA user_version").fetchone() == (2,)
        assert db.execute("SELECT name FROM machines WHERE id='machine'").fetchone() == ("legacy-machine",)
        assert db.execute("SELECT runtime_pause_id FROM runtime_bindings WHERE id='binding'").fetchone() == (
            "legacy-pause-id",)
        assert db.execute("SELECT count(*) FROM operations").fetchone() == (0,)
        assert db.execute("PRAGMA foreign_key_check").fetchall() == []
    print("old release opened its own pre-upgrade backup in an independent directory, issued a new "
          "control epoch, accepted the original owner, and rejected the stale public mutation (409)", flush=True)


def run(args, root):
    config, origin = configure(root)
    password = root / "owner-password"
    with open(password, "x", opener=lambda path, flags: os.open(path, flags, 0o600)) as stream:
        stream.write(secrets.token_urlsafe(32))
    database = root / "state/platform.db"
    owner = serve_once(args.old_platformd, config, origin, password)
    installation, epoch = add_legacy_pause(database)
    identity = database.stat()
    before = db_fingerprint(database)
    assert before[0] == 2
    archive = root / "schema2-backup"
    old_backup = admin(args.old_cli, config, "backup", "create", "--output", archive)
    assert old_backup["installation_id"] == installation and old_backup["control_epoch"] == epoch
    assert old_backup == admin(args.old_cli, config, "backup", "verify", "--archive", archive)
    assert db_fingerprint(archive / "platform.db") == before
    page_size, wal_bytes = interrupt_migration(args.platformd, config, database, args.wal_cut)
    assert db_fingerprint(database) == before, "interrupted migration changed the schema or a historical row"
    assert os.path.samestat(database.stat(), identity)
    print(f"SIGKILL after two uncommitted WAL headers, page_size={page_size} wal_bytes={wal_bytes}; "
          f"all {before[1]} schema-2 tables and rows recovered", flush=True)

    assert serve_once(args.old_platformd, config, origin, password) == owner
    assert serve_once(args.platformd, config, origin, password) == owner
    verify_current(database, args.schema, owner, installation, epoch)
    upgraded = db_fingerprint(database)
    old = subprocess.run([args.old_platformd, "--config", config], capture_output=True, text=True, timeout=8)
    assert old.returncode and f"unsupported registry schema version {args.schema}" in old.stderr, old.stderr
    assert db_fingerprint(database) == upgraded
    print(f"old release reopens after crash; new release migrates to schema {args.schema}; "
          "old binary refuses the upgraded registry without modifying it", flush=True)

    assert admin(args.cli, config, "backup", "verify", "--archive", archive) == old_backup
    admin(args.cli, config, "maintenance", "enter", "--reason", "verify same-installation rollback")
    rollback = root / "pre-restore"
    restored = admin(args.cli, config, "backup", "restore", "--archive", archive, "--rollback-output", rollback)
    assert restored["archive_epoch"] == epoch
    recovery = restored["control"]
    assert recovery["maintenance"] and recovery["control_epoch"] != epoch
    prior = admin(args.cli, config, "backup", "verify", "--archive", rollback)
    assert prior == restored["previous_backup"] and prior["schema_version"] == args.schema
    assert prior["control_epoch"] == epoch and prior["installation_id"] == installation
    verify_current(database, args.schema, owner, installation, recovery["control_epoch"])
    verify_current(rollback / "platform.db", args.schema, owner, installation, epoch)
    assert db_fingerprint(archive / "platform.db") == before
    admin(args.cli, config, "maintenance", "exit")
    assert serve_once(args.platformd, config, origin, password, epoch) == owner
    assert os.path.samestat(database.stat(), identity)
    print("release CLI restored the historical backup in place, rotated the control epoch, preserved the "
          "owner and pause ID, verified the pre-restore backup, and rejected the stale public mutation (409)",
          flush=True)
    recover_old_release(args, root, archive, password, owner, installation, epoch, recovery["control_epoch"])
    verify_current(database, args.schema, owner, installation, recovery["control_epoch"])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("old-platformd", "old-cli", "platformd", "cli", "wal-cut"):
        parser.add_argument("--" + flag, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--schema", type=int, required=True, help="schema_version in the new release manifest")
    parser.add_argument("--work-dir", type=Path, help="parent directory for a new disposable installation")
    arguments = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="registry-cut-", dir=arguments.work_dir) as work:
        run(arguments, Path(work).resolve())
