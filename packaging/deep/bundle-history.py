#!/usr/bin/env python3
"""Inspect pinned RuntimeBundle readers and retain referenced installations."""

import argparse
from contextlib import ExitStack, closing, contextmanager
import fcntl
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import time

from bundle import MANIFEST, sync_path, verify_installed_bundle
from bundle_inputs import BundleError, digest, read_json, require


PLATFORM_SCHEMAS = {1, 2, 1001, 1002, 1005, 1006, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014, 1015, 1016, 1017}
WORKER_SCHEMAS = range(1, 23)
SECURITY = ("active", "legacy-allowed", "quarantined", "revoked")


def connect(path, *, writing=False):
    path = Path(path).resolve(strict=True)
    require(path.is_file(), f"database is not a regular file: {path}")
    mode = "rw" if writing else "ro"
    return sqlite3.connect(path.as_uri() + "?mode=" + mode, uri=True, timeout=2, isolation_level=None)


def platform_schema(db):
    version, = db.execute("PRAGMA user_version").fetchone()
    control = db.execute("SELECT schema_version FROM meta WHERE id=1").fetchone()
    require(version in PLATFORM_SCHEMAS and control == (version,), f"unknown platform schema {version}")
    return version


@contextmanager
def platform_writer(path):
    path = Path(path).resolve(strict=True)
    # This is the same advisory lock acquired by the registry's Go Store.
    with path.with_name(path.name + ".lock").open("rb") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise BundleError("platform registry is open; stop its writer before this maintenance operation") from error
        with closing(connect(path, writing=True)) as db:
            db.execute("PRAGMA foreign_keys=ON")
            db.execute("BEGIN IMMEDIATE")
            yield db


def count(db, query, bundle):
    return db.execute(query, (bundle,)).fetchone()[0]


def platform_references(db, bundle):
    version = platform_schema(db)
    if version <= 2:
        return {}
    refs = {"registered_routes": count(db, "SELECT COUNT(*) FROM runtime_instances WHERE runtime_bundle_id=?", bundle),
            "binding_records": count(db, "SELECT COUNT(*) FROM runtime_bindings WHERE runtime_bundle_id=?", bundle)}
    refs.update({f"root_{owner}": number for owner, number in db.execute(
        "SELECT owner_kind,COUNT(*) FROM runtime_bundle_refs WHERE bundle_id=? GROUP BY owner_kind", (bundle,))})
    if version >= 1009:
        refs["statepoint_receipts"] = count(db, "SELECT COUNT(*) FROM managed_statepoint_receipts WHERE runtime_bundle_id=?", bundle)
    return refs


def worker_references(db, bundle):
    version, = db.execute("PRAGMA user_version").fetchone()
    require(version in WORKER_SCHEMAS, f"unknown Worker schema {version}")
    refs = {
        "active_or_unknown_executions": count(db, "SELECT COUNT(*) FROM executions WHERE bundle_digest=? AND phase!='exited'", bundle),
        "pending_operations": count(db, "SELECT COUNT(*) FROM operations WHERE bundle_digest=? AND phase='NEW'", bundle),
        "rooted_resources": count(db, """WITH RECURSIVE closure(id) AS (
            SELECT resource_id FROM root_refs WHERE state='active'
            UNION SELECT child_resource_id FROM resource_edges JOIN closure ON closure.id=owner_resource_id
        ) SELECT COUNT(*) FROM closure JOIN resources r ON r.resource_id=closure.id
           JOIN operations o ON o.operation_id=r.created_by_operation WHERE o.bundle_digest=?""", bundle),
    }
    if version >= 10:
        refs["durable_or_exporting_statepoints"] = count(db, """SELECT COUNT(*) FROM statepoints s
            JOIN operations o ON o.operation_id=s.operation_id WHERE o.bundle_digest=? AND s.phase!='aborted'""", bundle)
    if version >= 17:
        refs["lifecycle_policies"] = count(db, "SELECT COUNT(*) FROM lifecycle_policies WHERE bundle_digest=?", bundle)
    if version >= 18:
        refs["pinned_backup_exports"] = count(db, "SELECT COUNT(*) FROM native_backup_exports WHERE bundle_digest=? AND phase='pinned'", bundle)
    return refs


def references(platform, worker, bundle):
    result = {"schema": "RuntimeBundleReferences/1", "bundle_digest": bundle,
              "platform": platform_references(platform, bundle), "worker": worker_references(worker, bundle)}
    result["can_retire"] = not any(result["platform"].values()) and not any(result["worker"].values())
    return result


def retire(platform_path, worker_path, installation, retired_root, bundle, *, allow_legacy_rootfs=False):
    verify_installed_bundle(installation, bundle, allow_legacy_rootfs=allow_legacy_rootfs)
    require(not installation.is_symlink(), "installation must be a real directory")
    original = installation.resolve(strict=True)
    target = retired_root.resolve(strict=True) / bundle.split(":", 1)[1]
    require(target != original and original not in target.parents, "retired root must be outside the installation")
    require(original.stat().st_dev == target.parent.stat().st_dev, "retirement must stay on the installation filesystem")
    require(not target.exists() and not target.is_symlink(), f"retired installation already exists: {target}")
    with platform_writer(platform_path) as platform, closing(connect(worker_path, writing=True)) as worker:
        worker.execute("BEGIN IMMEDIATE")
        state = references(platform, worker, bundle)
        if not state["can_retire"]:
            raise BundleError(f"Bundle is still referenced: {json.dumps(state, sort_keys=True)}")
        subprocess.run(["mv", "-nT", "--", original, target], check=True)
        require(not original.exists(), f"retired installation already exists: {target}")
        sync_path(original.parent)
        sync_path(target.parent)
    return {**state, "retired_path": str(target)}


def set_security(path, bundle, status, reason):
    require(status in SECURITY and 0 < len(reason.strip()) <= 1024, "security status and reason are required")
    with platform_writer(path) as db:
        require(platform_schema(db) >= 1014, "platform registry requires schema 1014 for Bundle security")
        updated = db.execute("UPDATE runtime_bundles SET security_status=? WHERE id=?", (status, bundle))
        require(updated.rowcount == 1, f"Bundle is not registered: {bundle}")
        db.execute("""INSERT INTO runtime_bundle_security(bundle_id,reason,reviewed_at) VALUES(?,?,?)
                     ON CONFLICT(bundle_id) DO UPDATE SET reason=excluded.reason,reviewed_at=excluded.reviewed_at""",
                   (bundle, reason.strip(), time.time_ns() // 1_000_000))
        db.commit()


def matrix(installations, states, platform, *, allow_legacy_rootfs=False):
    readers = []
    seen = set()
    for bundle, path in installations:
        expected = digest(bundle, "expected Bundle digest")
        require(expected not in seen, f"duplicate installed Bundle: {bundle}")
        seen.add(expected)
        installation = Path(path)
        verify_installed_bundle(installation, expected, allow_legacy_rootfs=allow_legacy_rootfs)
        manifest = read_json(installation / MANIFEST)
        security = "unregistered"
        if platform is not None and platform_schema(platform) >= 1001:
            record = platform.execute("SELECT security_status FROM runtime_bundles WHERE id=?", (expected,)).fetchone()
            if record:
                security, = record
        readers.append({"bundle_digest": expected, "state_reader_id": manifest["contracts"]["state_reader_id"],
                        "installation": str(installation.resolve()), "security_status": security,
                        "boot_profiles": manifest["guest"]["boot_profiles"], "host_requirements": manifest["host_requirements"]})
    eligibility = []
    for state_id, bundle, reader in states:
        digest(bundle, "source Bundle digest")
        require(bool(reader.strip()), "source state reader ID is required")
        installed = next((item for item in readers if item["bundle_digest"] == bundle), None)
        if installed is None:
            disposition = "bundle_missing"
        elif installed["state_reader_id"] != reader:
            disposition = "reader_mismatch"
        elif installed["security_status"] not in ("unregistered", "unknown", "ready", "active", "legacy-allowed"):
            disposition = "isolated_review_required"
        else:
            disposition = "exact_reader_available"
        eligibility.append({"state_id": state_id, "bundle_digest": bundle, "state_reader_id": reader,
                            "disposition": disposition})
    return {"schema": "RuntimeBundleReaderMatrix/1", "readers": readers, "states": eligibility}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("references", "retire"):
        command = commands.add_parser(name)
        command.add_argument("--registry", required=True, type=Path)
        command.add_argument("--worker-catalog", required=True, type=Path)
        command.add_argument("--bundle", required=True)
        if name == "retire":
            command.add_argument("--installation", required=True, type=Path)
            command.add_argument("--retired-root", required=True, type=Path)
            command.add_argument("--allow-legacy-rootfs", action="store_true")
    readers = commands.add_parser("readers")
    readers.add_argument("--registry", type=Path)
    readers.add_argument("--installed", nargs=2, metavar=("DIGEST", "PATH"), action="append", default=[])
    readers.add_argument("--state", nargs=3, metavar=("ID", "DIGEST", "READER"), action="append", default=[])
    readers.add_argument("--allow-legacy-rootfs", action="store_true")
    security = commands.add_parser("set-security")
    security.add_argument("--registry", required=True, type=Path)
    security.add_argument("--bundle", required=True)
    security.add_argument("--status", required=True, choices=SECURITY)
    security.add_argument("--reason", required=True)
    args = parser.parse_args()
    try:
        if args.command == "readers":
            with ExitStack() as stack:
                platform = stack.enter_context(closing(connect(args.registry))) if args.registry else None
                result = matrix(args.installed, args.state, platform, allow_legacy_rootfs=args.allow_legacy_rootfs)
        else:
            bundle = digest(args.bundle, "expected Bundle digest")
            if args.command == "set-security":
                set_security(args.registry, bundle, args.status, args.reason)
                return 0
            if args.command == "retire":
                result = retire(args.registry, args.worker_catalog, args.installation, args.retired_root, bundle,
                                allow_legacy_rootfs=args.allow_legacy_rootfs)
            else:
                with closing(connect(args.registry)) as platform, closing(connect(args.worker_catalog)) as worker:
                    platform.execute("BEGIN")
                    worker.execute("BEGIN")
                    result = references(platform, worker, bundle)
        print(json.dumps(result, indent=2, sort_keys=True))
    except (BundleError, OSError, ValueError, sqlite3.Error, subprocess.CalledProcessError) as error:
        print(f"Bundle history: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
