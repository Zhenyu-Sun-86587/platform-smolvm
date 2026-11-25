CREATE TABLE tasks (
 id TEXT PRIMARY KEY, machine_id TEXT NOT NULL REFERENCES machines(id),
 binding_generation INTEGER NOT NULL, task_namespace TEXT NOT NULL,
 guest_task_id TEXT NOT NULL, start_token TEXT NOT NULL, spec_hash TEXT NOT NULL,
 spec_json TEXT NOT NULL CHECK(json_valid(spec_json)), state TEXT NOT NULL,
 exit_code INTEGER, origin_machine_id TEXT, origin_task_id TEXT, observed_at INTEGER,
 result_json TEXT CHECK(result_json IS NULL OR json_valid(result_json)),
 UNIQUE(task_namespace,start_token), UNIQUE(task_namespace,origin_machine_id,origin_task_id)
);
CREATE INDEX tasks_by_machine ON tasks(machine_id,binding_generation);
CREATE TABLE exec_sessions (
 id TEXT PRIMARY KEY, machine_id TEXT NOT NULL REFERENCES machines(id),
 binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
 upstream_exec_id TEXT NOT NULL UNIQUE, principal_id TEXT NOT NULL REFERENCES principals(id),
 spec_hash TEXT NOT NULL, state TEXT NOT NULL, exit_code INTEGER,
 output_start INTEGER NOT NULL DEFAULT 0, output_end INTEGER NOT NULL DEFAULT 0,
 last_observed_at INTEGER
);
CREATE TABLE leases (
 id TEXT PRIMARY KEY, machine_id TEXT NOT NULL REFERENCES machines(id),
 binding_generation INTEGER NOT NULL, principal_id TEXT NOT NULL REFERENCES principals(id),
 token_hash TEXT NOT NULL, reason TEXT NOT NULL, expires_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','suspect','released')), revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE services (
 id TEXT PRIMARY KEY, machine_id TEXT NOT NULL REFERENCES machines(id),
 name TEXT NOT NULL, revision INTEGER NOT NULL, kind TEXT NOT NULL,
 guest_port INTEGER NOT NULL CHECK(guest_port BETWEEN 1 AND 65535), public_host TEXT UNIQUE,
 public_port INTEGER UNIQUE, readiness_json TEXT NOT NULL CHECK(json_valid(readiness_json)),
 access_json TEXT NOT NULL CHECK(json_valid(access_json)), enabled INTEGER NOT NULL,
 UNIQUE(machine_id,name)
);
CREATE TABLE port_allocations (
 id TEXT PRIMARY KEY, binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
 service_id TEXT REFERENCES services(id), protocol TEXT NOT NULL, host_port INTEGER NOT NULL,
 guest_port INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('allocated','bound','unknown','released'))
);
CREATE UNIQUE INDEX active_backend_port ON port_allocations(protocol,host_port) WHERE state!='released';
CREATE TABLE dependencies (
 parent_binding_id TEXT NOT NULL REFERENCES runtime_bindings(id), child_binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
 kind TEXT NOT NULL CHECK(kind IN ('logical_origin','live_branch_resource')), observed_at INTEGER,
 status TEXT NOT NULL CHECK(status IN ('active','released','unknown')),
 PRIMARY KEY(parent_binding_id,child_binding_id,kind), CHECK(parent_binding_id!=child_binding_id)
);
CREATE TABLE snapshots (
 id TEXT PRIMARY KEY, source_machine_id TEXT REFERENCES machines(id), source_generation INTEGER,
 purpose TEXT NOT NULL, task_content TEXT NOT NULL, state TEXT NOT NULL
 CHECK(state IN ('staging','ready','quarantined','deleting','deleted')),
 relative_path TEXT NOT NULL UNIQUE,
 sha256 TEXT, size_bytes INTEGER, manifest_json TEXT, provenance TEXT NOT NULL,
 signer_key_id TEXT, capture_receipt_json TEXT, closure_verified_at INTEGER,
 restore_verified_at INTEGER, created_at INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 label TEXT NOT NULL DEFAULT ''
);
CREATE TABLE snapshot_pins (
 snapshot_id TEXT NOT NULL REFERENCES snapshots(id), owner_kind TEXT NOT NULL, owner_id TEXT NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(snapshot_id,owner_kind,owner_id)
);
