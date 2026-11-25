ALTER TABLE operations ADD COLUMN source_disposition TEXT
    CHECK (source_disposition IN ('RUNNING', 'STANDBY', 'HELD', 'EXITED', 'UNKNOWN'));

CREATE TABLE operation_children (
    operation_id TEXT NOT NULL REFERENCES operations(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    state TEXT NOT NULL CHECK (length(state) > 0),
    resource_independence TEXT CHECK (resource_independence IN ('unknown', 'verified', 'failed')),
    error_code TEXT,
    PRIMARY KEY (operation_id, ordinal),
    UNIQUE (operation_id, binding_id)
);

CREATE TRIGGER operation_child_insert BEFORE INSERT ON operation_children
WHEN NOT EXISTS (
    SELECT 1 FROM operations o JOIN runtime_bindings source ON source.id = o.binding_id
        AND source.machine_id = o.machine_id AND source.generation = o.binding_generation
    JOIN runtime_bindings child ON child.id = NEW.binding_id
    WHERE o.id = NEW.operation_id AND o.kind = 'fork_batch'
        AND source.machine_id <> child.machine_id AND source.runtime_bundle_id = child.runtime_bundle_id
        AND source.runtime_instance_id = child.runtime_instance_id
)
BEGIN
    SELECT RAISE(ABORT, 'operation child differs from fork batch provenance');
END;

CREATE TRIGGER operation_child_identity_immutable
BEFORE UPDATE OF operation_id, ordinal, binding_id ON operation_children
BEGIN
    SELECT RAISE(ABORT, 'operation child identity is immutable');
END;

CREATE TABLE vm_snapshot_provenance (
    snapshot_id TEXT PRIMARY KEY NOT NULL REFERENCES snapshots(id),
    operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id),
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    runtime_bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    state_reader_id TEXT NOT NULL CHECK (length(state_reader_id) > 0),
    runtime_capture_id TEXT NOT NULL CHECK (length(runtime_capture_id) > 0),
    UNIQUE (runtime_instance_id, runtime_capture_id)
);

CREATE TRIGGER vm_snapshot_provenance_insert BEFORE INSERT ON vm_snapshot_provenance
WHEN NOT EXISTS (
    SELECT 1 FROM operations o
    JOIN runtime_bindings b ON b.id = o.binding_id AND b.machine_id = o.machine_id AND b.generation = o.binding_generation
    JOIN runtime_bundles rb ON rb.id = b.runtime_bundle_id
    JOIN snapshots s ON s.id = NEW.snapshot_id AND s.source_machine_id = b.machine_id
        AND s.source_generation = b.generation
    WHERE o.id = NEW.operation_id AND o.kind = 'vm_snapshot'
        AND b.runtime_instance_id = NEW.runtime_instance_id AND b.runtime_bundle_id = NEW.runtime_bundle_id
        AND json_extract(rb.manifest_json, '$.contracts.state_reader_id') = NEW.state_reader_id
)
BEGIN
    SELECT RAISE(ABORT, 'VM snapshot provenance differs from capture binding');
END;

CREATE TRIGGER vm_snapshot_pin_bundle AFTER INSERT ON vm_snapshot_provenance
BEGIN
    INSERT INTO runtime_bundle_refs (bundle_id, owner_kind, owner_id)
    VALUES (NEW.runtime_bundle_id, 'snapshot', NEW.snapshot_id);
END;

CREATE TRIGGER vm_snapshot_provenance_immutable BEFORE UPDATE ON vm_snapshot_provenance
BEGIN
    SELECT RAISE(ABORT, 'VM snapshot provenance is immutable');
END;
