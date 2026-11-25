CREATE TABLE machine_lifecycle_policies (
    machine_id TEXT PRIMARY KEY REFERENCES machines(id),
    desired_revision INTEGER NOT NULL CHECK(desired_revision>0),
    applied_revision INTEGER NOT NULL DEFAULT 0 CHECK(applied_revision>=0 AND applied_revision<=desired_revision),
    desired_json TEXT NOT NULL CHECK(json_valid(desired_json)),
    overrides_json TEXT NOT NULL DEFAULT '{}' CHECK(json_type(overrides_json)='object'),
    applied_json TEXT CHECK(json_valid(applied_json)),
    source TEXT NOT NULL,
    default_revision INTEGER NOT NULL DEFAULT 0,
    last_operation TEXT NOT NULL,
    worker_status TEXT CHECK(json_valid(worker_status)),
    last_error TEXT
);
INSERT INTO machine_lifecycle_policies(machine_id,desired_revision,desired_json,source,last_operation)
SELECT id,1,json_object('auto_standby',json('false'),
       'auto_hibernate',json(CASE WHEN json_extract(idle_policy_json,'$.enabled')=1 THEN 'true' ELSE 'false' END),
       'hibernate_after_seconds',coalesce(nullif(json_extract(idle_policy_json,'$.timeout_seconds'),0),1800),
       'keep_running',json(CASE WHEN keep_running=1 THEN 'true' ELSE 'false' END)),
       'legacy','migration' FROM machines;
UPDATE machine_lifecycle_policies SET overrides_json=desired_json WHERE source='legacy';
ALTER TABLE leases ADD COLUMN semantics_version INTEGER NOT NULL DEFAULT 1 CHECK(semantics_version IN(1,2));
ALTER TABLE leases ADD COLUMN execution_epoch INTEGER;
ALTER TABLE leases ADD COLUMN incarnation_id TEXT;
ALTER TABLE leases ADD COLUMN worker_revision INTEGER;
ALTER TABLE leases ADD COLUMN release_reason TEXT;
CREATE TABLE lifecycle_deliveries (
    id TEXT PRIMARY KEY,machine_id TEXT NOT NULL REFERENCES machines(id),binding_id TEXT NOT NULL,
    desired_revision INTEGER NOT NULL,policy_json TEXT NOT NULL,expected_json TEXT NOT NULL,
    delivered_at INTEGER
);
CREATE TABLE lifecycle_outbox_cursors (
    machine_id TEXT PRIMARY KEY REFERENCES machines(id),seq INTEGER NOT NULL
);
CREATE TABLE lifecycle_lease_intents (
    id TEXT PRIMARY KEY, lease_id TEXT NOT NULL, machine_id TEXT NOT NULL REFERENCES machines(id), binding_id TEXT NOT NULL,
    method TEXT NOT NULL, principal TEXT NOT NULL, credential TEXT NOT NULL, scope_json TEXT NOT NULL, request_json TEXT NOT NULL,
    secret BLOB NOT NULL, lease_revision INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'pending', receipt_json TEXT,
    created_at INTEGER NOT NULL, token_delivered INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX lifecycle_one_lease_mutation ON lifecycle_lease_intents(lease_id) WHERE state='pending';
CREATE TABLE lifecycle_policy_applications (
    id TEXT PRIMARY KEY, default_revision INTEGER NOT NULL,
    policy_json TEXT NOT NULL CHECK(json_valid(policy_json)), created_at INTEGER NOT NULL
);
CREATE TABLE lifecycle_policy_application_targets (
    application_id TEXT NOT NULL REFERENCES lifecycle_policy_applications(id),
    ordinal INTEGER NOT NULL, machine_id TEXT NOT NULL, expected_revision INTEGER NOT NULL,
    binding_id TEXT, policy_revision INTEGER, state TEXT NOT NULL CHECK(state IN ('pending','applied','rejected')),
    reason TEXT, PRIMARY KEY(application_id,ordinal), UNIQUE(application_id,machine_id)
);
CREATE INDEX lifecycle_pending_targets ON lifecycle_policy_application_targets(machine_id,policy_revision) WHERE state='pending';
