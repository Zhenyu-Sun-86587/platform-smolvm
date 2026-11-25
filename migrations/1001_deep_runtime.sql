-- Deep registries share the Phase 1.5 schema through version 2.
-- A separate version range prevents a Phase 2 registry being opened as deep.
CREATE TABLE runtime_bundles (
    id TEXT PRIMARY KEY NOT NULL,
    manifest_digest TEXT NOT NULL UNIQUE,
    manifest_json TEXT NOT NULL CHECK (json_valid(manifest_json)),
    verification_status TEXT NOT NULL,
    security_status TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE runtime_instances (
    id TEXT PRIMARY KEY NOT NULL,
    lane TEXT NOT NULL CHECK (lane = 'deep'),
    runtime_bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    endpoint_key TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0)
);
CREATE TABLE runtime_bundle_refs (
    bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    owner_kind TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    PRIMARY KEY(bundle_id, owner_kind, owner_id)
);
ALTER TABLE runtime_bindings ADD COLUMN runtime_instance_id TEXT REFERENCES runtime_instances(id);
ALTER TABLE runtime_bindings ADD COLUMN runtime_bundle_id TEXT REFERENCES runtime_bundles(id)
    CHECK ((runtime_bundle_id IS NULL) = (runtime_instance_id IS NULL));
ALTER TABLE runtime_bindings ADD COLUMN runtime_instance_object_id TEXT;
ALTER TABLE runtime_bindings ADD COLUMN incarnation_id TEXT;
ALTER TABLE runtime_bindings ADD COLUMN execution_epoch INTEGER
    CHECK (execution_epoch IS NULL OR (typeof(execution_epoch) = 'integer' AND execution_epoch > 0))
    CHECK ((execution_epoch IS NULL) = (incarnation_id IS NULL))
    CHECK (execution_epoch IS NULL OR (runtime_instance_object_id IS NOT NULL AND runtime_instance_id IS NOT NULL));
ALTER TABLE runtime_bindings ADD COLUMN power_state TEXT NOT NULL DEFAULT 'unknown'
    CHECK (power_state IN ('unknown','running','standby','hibernated','stopped'));
ALTER TABLE runtime_bindings ADD COLUMN health_json TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(health_json) AND json_type(health_json) = 'object');
ALTER TABLE runtime_bindings ADD COLUMN capability_profile_id TEXT;
ALTER TABLE runtime_bindings ADD COLUMN last_capture_id TEXT;

-- Bundle identity is immutable for a Serve endpoint. Every binding with a
-- custom instance must pin the same bundle that the Serve endpoint runs.
CREATE TRIGGER immutable_runtime_instance_bundle BEFORE UPDATE OF runtime_bundle_id ON runtime_instances
WHEN NEW.runtime_bundle_id <> OLD.runtime_bundle_id
BEGIN
    SELECT RAISE(ABORT, 'runtime instance bundle is immutable');
END;
CREATE TRIGGER binding_bundle_insert BEFORE INSERT ON runtime_bindings
WHEN NEW.runtime_bundle_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM runtime_instances WHERE id = NEW.runtime_instance_id AND runtime_bundle_id = NEW.runtime_bundle_id
)
BEGIN
    SELECT RAISE(ABORT, 'binding bundle differs from runtime instance');
END;
CREATE TRIGGER binding_bundle_update BEFORE UPDATE OF runtime_instance_id, runtime_bundle_id ON runtime_bindings
WHEN NEW.runtime_bundle_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM runtime_instances WHERE id = NEW.runtime_instance_id AND runtime_bundle_id = NEW.runtime_bundle_id
)
BEGIN
    SELECT RAISE(ABORT, 'binding bundle differs from runtime instance');
END;
