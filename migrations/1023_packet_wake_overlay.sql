ALTER TABLE worker_managed_packet_rules ADD COLUMN source_binding_id TEXT REFERENCES runtime_bindings(id);
ALTER TABLE worker_managed_packet_rules ADD COLUMN desired_enabled INTEGER NOT NULL DEFAULT 0 CHECK(desired_enabled IN (0,1));
ALTER TABLE worker_managed_packet_rules ADD COLUMN intent_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE worker_managed_packet_rules ADD COLUMN overlay_json TEXT CHECK(overlay_json IS NULL OR json_valid(overlay_json));
