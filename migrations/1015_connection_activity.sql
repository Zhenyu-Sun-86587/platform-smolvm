CREATE TABLE connection_activity (
    id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    principal TEXT NOT NULL,
    execution_json TEXT NOT NULL CHECK(json_valid(execution_json)),
    owner_session TEXT NOT NULL,
    closing INTEGER NOT NULL DEFAULT 0 CHECK(closing IN (0,1)),
    last_attempt INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX connection_activity_retry ON connection_activity(last_attempt);

CREATE TRIGGER lifecycle_target_replaced AFTER UPDATE OF current_binding_id,deleted_at ON machines
WHEN NEW.deleted_at IS NOT NULL OR OLD.current_binding_id IS NOT NEW.current_binding_id
BEGIN
    UPDATE lifecycle_policy_application_targets
       SET state='rejected',reason=CASE WHEN NEW.deleted_at IS NOT NULL THEN 'MACHINE_DELETED' ELSE 'BINDING_REPLACED' END
     WHERE machine_id=NEW.id AND state='pending'
       AND (NEW.deleted_at IS NOT NULL OR binding_id IS NOT NEW.current_binding_id);
END;
