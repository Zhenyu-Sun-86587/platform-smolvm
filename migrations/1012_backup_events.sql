ALTER TABLE events ADD COLUMN resource_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN resource_id TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN event_name TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version > 0);
ALTER TABLE events ADD COLUMN audience TEXT NOT NULL DEFAULT 'legacy' CHECK (audience IN ('legacy','read','admin','owner'));
ALTER TABLE events ADD COLUMN audience_owner TEXT;

CREATE TABLE backup_runs (
    id TEXT PRIMARY KEY,
    policy_id TEXT NOT NULL,
    control_epoch TEXT NOT NULL,
    scope TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running','verified','failed','retiring','retired')),
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    failure_code TEXT
);

CREATE UNIQUE INDEX one_backup_run_in_progress ON backup_runs(policy_id) WHERE state='running';
CREATE INDEX backup_runs_by_policy ON backup_runs(policy_id,started_at DESC,id);
