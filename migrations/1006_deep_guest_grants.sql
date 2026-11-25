CREATE TABLE guest_identities (
    machine_id TEXT PRIMARY KEY REFERENCES machines(id),
    principal_id TEXT NOT NULL UNIQUE REFERENCES principals(id),
    owner_principal_id TEXT NOT NULL REFERENCES principals(id)
);

CREATE TABLE guest_grants (
    id TEXT PRIMARY KEY,
    source_machine_id TEXT NOT NULL REFERENCES guest_identities(machine_id),
    action TEXT NOT NULL CHECK (action IN ('machine.create','machine.fork','machine.exec','machine.read')),
    target_machine_id TEXT REFERENCES machines(id),
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER,
    revision INTEGER NOT NULL CHECK (revision > 0),
    maximum_uses INTEGER NOT NULL CHECK (maximum_uses > 0),
    used_count INTEGER NOT NULL DEFAULT 0 CHECK (used_count BETWEEN 0 AND maximum_uses),
    CHECK ((action = 'machine.create') = (target_machine_id IS NULL))
);

CREATE TABLE guest_credentials (
    id TEXT PRIMARY KEY,
    digest TEXT NOT NULL UNIQUE,
    grant_id TEXT NOT NULL REFERENCES guest_grants(id),
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    runtime_bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    runtime_object_id TEXT NOT NULL,
    incarnation_id TEXT NOT NULL,
    execution_epoch INTEGER NOT NULL CHECK (execution_epoch > 0),
    expires_at INTEGER NOT NULL
);
