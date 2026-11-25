CREATE TABLE volumes (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES principals(id),
    name TEXT NOT NULL,
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    runtime_bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    revision INTEGER NOT NULL CHECK(revision > 0),
    state TEXT NOT NULL CHECK(state IN ('available','deleted')),
    worker_receipt_json TEXT NOT NULL CHECK(json_valid(worker_receipt_json)),
    created_at INTEGER NOT NULL
);

CREATE INDEX volume_owner_index ON volumes(owner_id,state,id);

CREATE TABLE volume_attachments (
    id TEXT PRIMARY KEY,
    volume_id TEXT NOT NULL REFERENCES volumes(id),
    machine_id TEXT NOT NULL REFERENCES machines(id),
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    state TEXT NOT NULL CHECK(state IN ('reserved','detached')),
    revision INTEGER NOT NULL CHECK(revision > 0)
);

CREATE INDEX machine_volume_index ON volume_attachments(machine_id,state,id);

CREATE TABLE volume_snapshots (
    id TEXT PRIMARY KEY,
    volume_id TEXT NOT NULL REFERENCES volumes(id),
    generation_handle TEXT NOT NULL,
    source_revision INTEGER NOT NULL CHECK(source_revision > 0),
    state TEXT NOT NULL CHECK(state IN ('available','deleted')),
    pin_receipt_json TEXT NOT NULL CHECK(json_valid(pin_receipt_json))
);
