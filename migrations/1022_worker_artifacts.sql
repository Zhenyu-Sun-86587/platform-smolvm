CREATE TABLE artifact_placements (
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    content_digest TEXT NOT NULL,
    local_generation_id TEXT NOT NULL,
    capacity_bytes INTEGER NOT NULL CHECK(capacity_bytes > 0),
    verified_at_ms INTEGER NOT NULL,
    PRIMARY KEY(runtime_instance_id, content_digest),
    UNIQUE(runtime_instance_id, local_generation_id)
);
