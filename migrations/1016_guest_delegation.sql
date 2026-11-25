ALTER TABLE guest_credentials RENAME TO guest_credentials_1006;
ALTER TABLE guest_grants RENAME TO guest_grants_1006;

CREATE TABLE guest_grants (
    id TEXT PRIMARY KEY,
    source_machine_id TEXT NOT NULL REFERENCES guest_identities(machine_id),
    action TEXT NOT NULL CHECK (action IN ('machine.create','machine.fork','machine.exec','machine.read','credential_proxy.use')),
    target_machine_id TEXT REFERENCES machines(id),
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER,
    revision INTEGER NOT NULL CHECK (revision > 0),
    maximum_uses INTEGER NOT NULL CHECK (maximum_uses > 0),
    used_count INTEGER NOT NULL DEFAULT 0 CHECK (used_count BETWEEN 0 AND maximum_uses),
    proxy_id TEXT,
    maximum_concurrent INTEGER NOT NULL DEFAULT 4 CHECK (maximum_concurrent BETWEEN 1 AND 64),
    limits_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(limits_json) AND json_type(limits_json)='object'),
    CHECK ((action IN ('machine.create','credential_proxy.use')) = (target_machine_id IS NULL)),
    CHECK ((action = 'credential_proxy.use') = (proxy_id IS NOT NULL))
);
INSERT INTO guest_grants (id,source_machine_id,action,target_machine_id,expires_at,revoked_at,revision,maximum_uses,used_count)
SELECT id,source_machine_id,action,target_machine_id,expires_at,revoked_at,revision,maximum_uses,used_count FROM guest_grants_1006;

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
INSERT INTO guest_credentials SELECT * FROM guest_credentials_1006;
DROP TABLE guest_credentials_1006;
DROP TABLE guest_grants_1006;

CREATE TABLE guest_operation_authority (
    operation_id TEXT PRIMARY KEY REFERENCES operations(id),
    credential_id TEXT NOT NULL REFERENCES guest_credentials(id),
    grant_id TEXT NOT NULL REFERENCES guest_grants(id),
    grant_revision INTEGER NOT NULL CHECK (grant_revision > 0)
);
CREATE INDEX guest_operations_by_grant ON guest_operation_authority(grant_id);

CREATE TABLE guest_created_machines (
    machine_id TEXT PRIMARY KEY REFERENCES machines(id),
    owner_principal_id TEXT NOT NULL REFERENCES principals(id),
    creator_principal_id TEXT NOT NULL REFERENCES principals(id),
    reserved_memory_mib INTEGER NOT NULL CHECK (reserved_memory_mib > 0)
);
CREATE INDEX guest_machines_by_creator ON guest_created_machines(creator_principal_id,owner_principal_id);

CREATE TABLE guest_call_leases (
    id TEXT PRIMARY KEY,
    grant_id TEXT NOT NULL REFERENCES guest_grants(id),
    expires_at INTEGER NOT NULL
);
CREATE INDEX guest_calls_by_grant ON guest_call_leases(grant_id,expires_at);

CREATE TABLE guest_proxy_rate (
    grant_id TEXT PRIMARY KEY REFERENCES guest_grants(id),
    window INTEGER NOT NULL,
    uses INTEGER NOT NULL CHECK (uses > 0)
);
