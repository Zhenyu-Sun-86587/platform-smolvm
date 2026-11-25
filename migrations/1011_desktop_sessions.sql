CREATE TABLE desktop_sessions (
    id TEXT PRIMARY KEY,
    lease_id TEXT NOT NULL UNIQUE REFERENCES leases(id),
    service_id TEXT NOT NULL,
    service_revision INTEGER NOT NULL CHECK (service_revision > 0),
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    execution_epoch INTEGER NOT NULL CHECK (execution_epoch > 0),
    credential_id TEXT NOT NULL,
    control_epoch TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX desktop_session_credential ON desktop_sessions(credential_id, created_at);
