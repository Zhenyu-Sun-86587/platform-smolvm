CREATE TABLE runtime_bundle_security (
    bundle_id TEXT PRIMARY KEY REFERENCES runtime_bundles(id),
    reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
    reviewed_at INTEGER NOT NULL CHECK (reviewed_at > 0)
);
