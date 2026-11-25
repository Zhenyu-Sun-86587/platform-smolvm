"""Check historical reader decisions and catalog pins before Bundle retirement."""

from contextlib import closing
import fcntl
from pathlib import Path
import runpy
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "packaging/deep"))
history = runpy.run_path(str(ROOT / "packaging/deep/bundle-history.py"))
BundleError = history["BundleError"]
BUNDLE = "sha256:" + "a" * 64
OTHER = "sha256:" + "b" * 64


class BundleHistoryTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        self.registry = root / "registry.sqlite"
        self.lock = self.registry.with_name(self.registry.name + ".lock")
        self.lock.touch()
        self.catalog = root / "catalog.sqlite"
        self.installation = root / "installed"
        self.installation.mkdir()
        self.retired = root / "retired"
        self.retired.mkdir()
        with closing(sqlite3.connect(self.registry)) as db:
            for migration in sorted((ROOT / "migrations").glob("*.sql")):
                db.executescript(migration.read_text())
            db.execute("PRAGMA user_version=1014")
            db.execute("INSERT INTO meta (id,schema_version,installation_id,control_epoch) VALUES(1,1014,'fixture','epoch')")
            for bundle in (BUNDLE, OTHER):
                db.execute("INSERT INTO runtime_bundles VALUES (?,?,'{}','verified','active',1)", (bundle, bundle))
            db.commit()
        with closing(sqlite3.connect(self.catalog)) as db:
            db.executescript("""PRAGMA user_version=17;
                CREATE TABLE executions(bundle_digest,phase);
                CREATE TABLE operations(operation_id,bundle_digest,phase);
                CREATE TABLE resources(resource_id,created_by_operation);
                CREATE TABLE root_refs(resource_id,state);
                CREATE TABLE resource_edges(owner_resource_id,child_resource_id);
                CREATE TABLE statepoints(operation_id,phase);
                CREATE TABLE lifecycle_policies(bundle_digest);
                """)

    def references(self):
        with closing(history["connect"](self.registry)) as registry, closing(history["connect"](self.catalog)) as catalog:
            return history["references"](registry, catalog, BUNDLE)

    def retire(self):
        handler = history["retire"]
        with patch.dict(handler.__globals__, {"verify_installed_bundle": lambda *args, **kwargs: BUNDLE}):
            return handler(self.registry, self.catalog, self.installation, self.retired, BUNDLE)

    def test_historical_state_blocks_retirement_until_its_references_are_dropped(self):
        with sqlite3.connect(self.registry) as registry, sqlite3.connect(self.catalog) as catalog:
            registry.execute("INSERT INTO runtime_bundle_refs VALUES (?,'statepoint','history')", (BUNDLE,))
            catalog.executemany("INSERT INTO operations VALUES (?,?,'COMPLETED')", (("old", BUNDLE), ("new", OTHER)))
            catalog.execute("INSERT INTO statepoints VALUES ('old','durable')")
            catalog.executemany("INSERT INTO resources VALUES (?,?)", (("target", "new"), ("ancestor", "old")))
            catalog.execute("INSERT INTO resource_edges VALUES ('target','ancestor')")
            catalog.execute("INSERT INTO root_refs VALUES ('target','active')")
        state = self.references()
        self.assertEqual(state["platform"]["root_statepoint"], 1)
        self.assertEqual(state["worker"]["durable_or_exporting_statepoints"], 1)
        self.assertEqual(state["worker"]["rooted_resources"], 1)
        with self.assertRaisesRegex(BundleError, "still referenced"):
            self.retire()
        self.assertTrue(self.installation.exists())
        with sqlite3.connect(self.registry) as registry, sqlite3.connect(self.catalog) as catalog:
            registry.execute("DELETE FROM runtime_bundle_refs")
            catalog.execute("UPDATE statepoints SET phase='aborted'")
            catalog.execute("UPDATE root_refs SET state='dropped'")
        retired = self.retire()
        self.assertFalse(self.installation.exists())
        self.assertTrue(Path(retired["retired_path"]).is_dir())

    def test_unknown_worker_schema_refuses_retirement(self):
        with sqlite3.connect(self.catalog) as catalog:
            catalog.execute("PRAGMA user_version=999")
        with self.assertRaisesRegex(BundleError, "unknown Worker schema"):
            self.retire()
        self.assertTrue(self.installation.exists())

    def test_current_worker_keeps_reader_for_retained_backup(self):
        with sqlite3.connect(self.catalog) as db:
            db.execute("PRAGMA user_version=22")
            db.execute("CREATE TABLE native_backup_exports(bundle_digest,phase)")
            db.execute("INSERT INTO native_backup_exports VALUES (?,'pinned')", (BUNDLE,))
        self.assertEqual(self.references()["worker"]["pinned_backup_exports"], 1)
        with self.assertRaisesRegex(BundleError, "still referenced"):
            self.retire()
        with sqlite3.connect(self.catalog) as db:
            db.execute("UPDATE native_backup_exports SET phase='released'")
        self.assertTrue(Path(self.retire()["retired_path"]).is_dir())

    def test_platform_writer_lock_and_quarantined_reader(self):
        with self.lock.open("rb") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(BundleError, "registry is open"):
                history["set_security"](self.registry, BUNDLE, "quarantined", "historical exposure")
        history["set_security"](self.registry, BUNDLE, "quarantined", "historical exposure")
        manifest = {"contracts": {"state_reader_id": "old-reader"}, "guest": {"boot_profiles": ["oci-v1"]},
                    "host_requirements": {}}
        handler = history["matrix"]
        with closing(history["connect"](self.registry)) as registry, patch.dict(handler.__globals__, {
                "verify_installed_bundle": lambda *args, **kwargs: BUNDLE, "read_json": lambda path: manifest}):
            report = handler([(BUNDLE, str(self.installation))], [("one", BUNDLE, "old-reader"),
                              ("two", BUNDLE, "other-reader"), ("three", OTHER, "old-reader")], registry)
            self.assertEqual(registry.execute("SELECT reason FROM runtime_bundle_security WHERE bundle_id=?", (BUNDLE,)).fetchone(),
                             ("historical exposure",))
        self.assertEqual([row["disposition"] for row in report["states"]],
                         ["isolated_review_required", "reader_mismatch", "bundle_missing"])


if __name__ == "__main__":
    unittest.main()
