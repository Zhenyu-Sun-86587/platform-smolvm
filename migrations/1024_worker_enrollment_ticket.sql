CREATE TABLE worker_enrollment_tickets (
    worker_id TEXT PRIMARY KEY NOT NULL REFERENCES workers(id),
    generation INTEGER NOT NULL CHECK (generation > 0),
    controller_id TEXT NOT NULL,
    control_epoch TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    spki_pin TEXT NOT NULL,
    ticket_hash BLOB NOT NULL CHECK (length(ticket_hash) = 32),
    expires_at INTEGER NOT NULL,
    prior_generation INTEGER NOT NULL CHECK (prior_generation >= 0 AND prior_generation < generation),
    prior_status TEXT NOT NULL CHECK (prior_status IN ('','active','revoked')),
    csr_hash BLOB CHECK (csr_hash IS NULL OR length(csr_hash) = 32),
    certificate BLOB,
    CHECK ((csr_hash IS NULL) = (certificate IS NULL))
);

CREATE TABLE worker_enrollment_key_revocations (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    generation INTEGER NOT NULL CHECK (generation > 0),
    spki_pin TEXT NOT NULL,
    revoked_at INTEGER NOT NULL,
    PRIMARY KEY (worker_id,generation,spki_pin)
);
