CREATE TABLE native_backup_selections (
    backup_id TEXT PRIMARY KEY REFERENCES backup_runs(id),
    catalog_sha256 TEXT NOT NULL CHECK(length(catalog_sha256)=64),
    worker_routes_json TEXT NOT NULL CHECK(json_valid(worker_routes_json) AND json_type(worker_routes_json)='array'),
    worker_pinned INTEGER NOT NULL DEFAULT 0 CHECK(worker_pinned IN (0,1))
);

CREATE TABLE native_backup_volume_pins (
    backup_id TEXT NOT NULL REFERENCES native_backup_selections(backup_id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL,
    PRIMARY KEY(snapshot_id,backup_id)
);
