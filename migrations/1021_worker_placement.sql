CREATE TABLE worker_sample_sessions (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    control_epoch TEXT NOT NULL,
    enrollment_generation INTEGER NOT NULL CHECK (enrollment_generation >= 0),
    host_boot_id TEXT NOT NULL,
    broker_session_id TEXT NOT NULL,
    node_session_id TEXT NOT NULL,
    latest_sequence INTEGER NOT NULL CHECK (latest_sequence > 0),
    PRIMARY KEY (worker_id, control_epoch, enrollment_generation, host_boot_id, broker_session_id, node_session_id)
);

CREATE TABLE worker_samples (
    worker_id TEXT NOT NULL PRIMARY KEY REFERENCES workers(id),
    control_epoch TEXT NOT NULL,
    enrollment_generation INTEGER NOT NULL,
    host_boot_id TEXT NOT NULL,
    broker_session_id TEXT NOT NULL,
    node_session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    query_nonce TEXT NOT NULL,
    query_started_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    oldest_sample_at INTEGER NOT NULL,
    report_json TEXT NOT NULL CHECK (json_valid(report_json)),
    FOREIGN KEY (worker_id, control_epoch, enrollment_generation, host_boot_id, broker_session_id, node_session_id)
        REFERENCES worker_sample_sessions(worker_id, control_epoch, enrollment_generation, host_boot_id, broker_session_id, node_session_id)
);

CREATE TABLE placement_reservations (
    operation_id TEXT NOT NULL REFERENCES operations(id),
    attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
    worker_id TEXT NOT NULL REFERENCES workers(id),
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    control_epoch TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    native_digest TEXT,
    vcpus INTEGER NOT NULL CHECK (vcpus >= 0),
    memory_mib INTEGER NOT NULL CHECK (memory_mib >= 0),
    storage_bytes INTEGER NOT NULL CHECK (storage_bytes >= 0),
    heavy_slots INTEGER NOT NULL CHECK (heavy_slots >= 0),
    phase TEXT NOT NULL CHECK (phase IN ('reserved_not_sent','dispatch_started','unknown','admitted','covered','terminal_rejected','released')),
    admitted_generation INTEGER,
    admitted_host_boot_id TEXT,
    admitted_broker_session_id TEXT,
    admitted_node_session_id TEXT,
    admitted_through_sequence INTEGER,
    admitted_receipt_id TEXT,
    covered_sequence INTEGER,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (operation_id, attempt)
);
CREATE INDEX placement_reservations_worker_phase ON placement_reservations(worker_id, phase);

CREATE TABLE worker_maintenance (
    worker_id TEXT NOT NULL PRIMARY KEY REFERENCES workers(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    phase TEXT NOT NULL,
    reason TEXT NOT NULL,
    requires_overlay_clear INTEGER NOT NULL DEFAULT 0 CHECK (requires_overlay_clear IN (0,1)),
    evidence_json TEXT CHECK (evidence_json IS NULL OR json_valid(evidence_json))
);

CREATE TABLE worker_storage_tombstones (
    worker_id TEXT NOT NULL PRIMARY KEY REFERENCES workers(id),
    maintenance_revision INTEGER NOT NULL,
    endpoint TEXT NOT NULL,
    enrollment_generation INTEGER NOT NULL,
    reason TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE worker_policy_overlays (
    binding_id TEXT NOT NULL PRIMARY KEY REFERENCES runtime_bindings(id),
    worker_id TEXT NOT NULL REFERENCES workers(id),
    maintenance_revision INTEGER NOT NULL,
    desired_revision INTEGER NOT NULL,
    worker_revision INTEGER NOT NULL,
    effective_json TEXT NOT NULL CHECK (json_valid(effective_json))
);
CREATE INDEX worker_policy_overlays_worker ON worker_policy_overlays(worker_id);

CREATE TABLE worker_authority_recovery (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    control_epoch TEXT NOT NULL,
    previous_enrollment_generation INTEGER NOT NULL,
    rebound_enrollment_generation INTEGER,
    evidence_json TEXT CHECK (evidence_json IS NULL OR json_valid(evidence_json)),
    PRIMARY KEY (worker_id, control_epoch)
);
