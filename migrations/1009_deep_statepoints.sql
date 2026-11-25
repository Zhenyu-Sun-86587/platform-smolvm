CREATE TABLE managed_statepoint_receipts (
    runtime_instance_id TEXT NOT NULL REFERENCES runtime_instances(id),
    capture_id TEXT NOT NULL,
    representation_id TEXT NOT NULL,
    runtime_bundle_id TEXT NOT NULL REFERENCES runtime_bundles(id),
    binding_id TEXT NOT NULL REFERENCES runtime_bindings(id),
    operation_id TEXT NOT NULL REFERENCES operations(id),
    purpose TEXT NOT NULL CHECK (purpose IN ('checkpoint','hibernate','rollback_rescue')),
    manifest_digest TEXT NOT NULL,
    receipt_json TEXT NOT NULL CHECK (json_valid(receipt_json)),
    PRIMARY KEY(runtime_instance_id,capture_id)
);
CREATE TRIGGER managed_statepoint_pin_bundle AFTER INSERT ON managed_statepoint_receipts
BEGIN
    INSERT OR IGNORE INTO runtime_bundle_refs(bundle_id,owner_kind,owner_id)
        VALUES(NEW.runtime_bundle_id,'statepoint',NEW.operation_id);
END;
