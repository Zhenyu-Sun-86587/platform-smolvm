ALTER TABLE operations ADD COLUMN source_lock_released INTEGER NOT NULL DEFAULT 0
    CHECK (source_lock_released IN (0, 1));

ALTER TABLE operation_children ADD COLUMN resource_proof_json TEXT NOT NULL DEFAULT '[]'
    CHECK (json_valid(resource_proof_json));

DROP TRIGGER operation_child_insert;
CREATE TRIGGER operation_child_insert BEFORE INSERT ON operation_children
WHEN NOT EXISTS (
    SELECT 1 FROM operations o JOIN runtime_bindings source ON source.id = o.binding_id
        AND source.machine_id = o.machine_id AND source.generation = o.binding_generation
    JOIN runtime_bindings child ON child.id = NEW.binding_id
    WHERE o.id = NEW.operation_id AND o.kind IN ('independent_fork', 'fork_batch')
        AND source.machine_id <> child.machine_id AND source.runtime_bundle_id = child.runtime_bundle_id
        AND source.runtime_instance_id = child.runtime_instance_id
)
BEGIN
    SELECT RAISE(ABORT, 'operation child differs from native fork provenance');
END;
