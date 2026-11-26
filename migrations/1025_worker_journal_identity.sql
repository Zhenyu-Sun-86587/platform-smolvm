CREATE TABLE worker_node_journals (
    worker_id TEXT PRIMARY KEY NOT NULL REFERENCES workers(id),
    journal_id TEXT NOT NULL UNIQUE CHECK (length(journal_id)=72 AND substr(journal_id,1,8)='journal-' AND substr(journal_id,9) NOT GLOB '*[^a-f0-9]*'),
    observed_node_session TEXT NOT NULL CHECK (length(observed_node_session) BETWEEN 1 AND 128)
);

CREATE TRIGGER immutable_worker_journal BEFORE UPDATE OF worker_id,journal_id ON worker_node_journals
BEGIN SELECT RAISE(ABORT,'Worker journal identity is immutable'); END;

CREATE TRIGGER retain_worker_journal BEFORE DELETE ON worker_node_journals
BEGIN SELECT RAISE(ABORT,'Worker journal identity remains pinned after decommission'); END;
