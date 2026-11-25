CREATE TABLE meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    schema_version INTEGER NOT NULL,
    installation_id TEXT NOT NULL,
    control_epoch TEXT NOT NULL,
    maintenance INTEGER NOT NULL DEFAULT 0 CHECK (maintenance IN (0, 1)),
    recovery_reason TEXT,
    settings_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(settings_json))
);

CREATE TABLE principals (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT,
    disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at INTEGER NOT NULL
);

CREATE TABLE credentials (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(id),
    kind TEXT NOT NULL CHECK (kind IN ('api_token', 'web_session', 'ssh_public_key', 'client_certificate')),
    digest TEXT NOT NULL UNIQUE,
    public_material TEXT,
    label TEXT NOT NULL DEFAULT '',
    scope_json TEXT NOT NULL CHECK (json_valid(scope_json)),
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    revoked_at INTEGER
);

CREATE TABLE templates (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    image_digest TEXT NOT NULL,
    guest_release TEXT,
    spec_json TEXT NOT NULL CHECK (json_valid(spec_json)),
    capabilities_json TEXT NOT NULL CHECK (json_valid(capabilities_json)),
    verified_at INTEGER,
    UNIQUE (name, version)
);

CREATE TABLE machines (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    template_id TEXT NOT NULL REFERENCES templates(id),
    revision INTEGER NOT NULL CHECK (revision >= 1),
    binding_generation INTEGER NOT NULL CHECK (binding_generation >= 0),
    desired_power TEXT NOT NULL,
    current_binding_id TEXT REFERENCES runtime_bindings(id),
    spec_json TEXT NOT NULL CHECK (json_valid(spec_json)),
    idle_policy_json TEXT NOT NULL CHECK (json_valid(idle_policy_json)),
    keep_running INTEGER NOT NULL CHECK (keep_running IN (0, 1)),
    last_user_activity_at INTEGER,
    deleted_at INTEGER
);

CREATE TABLE runtime_bindings (
    id TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines(id),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    runtime_name TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('candidate', 'current', 'retired', 'unknown')),
    observed_state TEXT NOT NULL DEFAULT 'unknown',
    native_state TEXT NOT NULL DEFAULT '',
    availability TEXT NOT NULL DEFAULT 'not_ready',
    runtime_identity_json TEXT CHECK (runtime_identity_json IS NULL OR json_valid(runtime_identity_json)),
    observed_at INTEGER,
    runtime_pause_id TEXT,
    pause_status TEXT CHECK (pause_status IN ('active', 'consumed', 'unknown')),
    task_namespace TEXT NOT NULL UNIQUE,
    spec_hash TEXT NOT NULL,
    ports_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(ports_json)),
    guest_management_status TEXT NOT NULL DEFAULT 'unknown' CHECK (guest_management_status IN ('ready', 'degraded', 'unknown')),
    guest_management_evidence_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(guest_management_evidence_json)),
    state_reason_json TEXT CHECK (state_reason_json IS NULL OR json_valid(state_reason_json)),
    UNIQUE (machine_id, generation)
);

CREATE UNIQUE INDEX one_current_binding ON runtime_bindings(machine_id) WHERE status = 'current';

CREATE TABLE operations (
    id TEXT PRIMARY KEY,
    control_epoch TEXT NOT NULL,
    principal_id TEXT REFERENCES principals(id),
    machine_id TEXT REFERENCES machines(id),
    source_machine_id TEXT REFERENCES machines(id),
    binding_id TEXT REFERENCES runtime_bindings(id),
    binding_generation INTEGER NOT NULL,
    runtime_name TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'dispatching', 'observing', 'succeeded', 'failed', 'cancelled', 'recovery_required')),
    stage TEXT NOT NULL,
    request_json TEXT NOT NULL CHECK (json_valid(request_json)),
    dispatch_json TEXT CHECK (dispatch_json IS NULL OR json_valid(dispatch_json)),
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    error_json TEXT CHECK (error_json IS NULL OR json_valid(error_json)),
    effect TEXT NOT NULL CHECK (effect IN ('none', 'applied', 'partial', 'unknown')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    heavy INTEGER NOT NULL CHECK (heavy IN (0, 1)),
    wake_pending INTEGER NOT NULL DEFAULT 0 CHECK (wake_pending IN (0, 1))
);

CREATE INDEX pending_operations ON operations(state, created_at);
CREATE INDEX machine_operations ON operations(machine_id, created_at);

CREATE TABLE operation_locks (
    machine_id TEXT PRIMARY KEY REFERENCES machines(id),
    operation_id TEXT NOT NULL REFERENCES operations(id),
    expected_generation INTEGER NOT NULL,
    role TEXT NOT NULL
);
CREATE INDEX operation_lock_owner ON operation_locks(operation_id);

CREATE TABLE idempotency (
    control_epoch TEXT NOT NULL,
    principal_id TEXT NOT NULL REFERENCES principals(id),
    scope TEXT NOT NULL,
    key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    operation_id TEXT REFERENCES operations(id),
    response_json TEXT CHECK (response_json IS NULL OR json_valid(response_json)),
    expires_at INTEGER,
    PRIMARY KEY (control_epoch, principal_id, scope, key)
);

CREATE TABLE events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    control_epoch TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('machine', 'operation', 'task', 'service', 'snapshot', 'lease', 'auth', 'maintenance')),
    machine_id TEXT REFERENCES machines(id),
    operation_id TEXT REFERENCES operations(id),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at INTEGER NOT NULL,
    audit INTEGER NOT NULL DEFAULT 0 CHECK (audit IN (0, 1))
);

CREATE INDEX events_by_epoch ON events(control_epoch, seq);
CREATE INDEX events_by_age ON events(audit, created_at);
