CREATE TABLE workers (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) BETWEEN 1 AND 128 AND id NOT GLOB '*[^a-zA-Z0-9_-]*'),
    name TEXT NOT NULL CHECK (length(name) > 0),
    endpoint TEXT NOT NULL DEFAULT '',
    desired_mode TEXT NOT NULL DEFAULT 'active'
        CHECK (desired_mode IN ('active','cordoned','draining','drained','decommissioned')),
    observed_status TEXT NOT NULL DEFAULT 'unknown'
        CHECK (observed_status IN ('unknown','registering','ready','degraded','unreachable','recovering','identity_conflict')),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0)
);
INSERT INTO workers(id,name) VALUES('local-worker','Local Worker');

CREATE TABLE worker_enrollments (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    generation INTEGER NOT NULL CHECK (generation > 0),
    certificate_fingerprint TEXT NOT NULL CHECK (length(certificate_fingerprint) > 0),
    authority TEXT NOT NULL CHECK (length(authority) > 0),
    status TEXT NOT NULL CHECK (status IN ('active','revoked')),
    created_at INTEGER NOT NULL,
    revoked_at INTEGER CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
    PRIMARY KEY (worker_id,generation)
);
CREATE UNIQUE INDEX worker_current_enrollment ON worker_enrollments(worker_id) WHERE status = 'active';
CREATE UNIQUE INDEX worker_current_certificate ON worker_enrollments(certificate_fingerprint) WHERE status = 'active';

-- Pre-upgrade databases were local to a single Worker. Preserve all global
-- IDs, including IDs referenced from backup manifests and older receipts.
ALTER TABLE runtime_instances ADD COLUMN worker_id TEXT NOT NULL DEFAULT 'local-worker';
ALTER TABLE runtime_instances ADD COLUMN local_instance_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_instances ADD COLUMN worker_endpoint_key TEXT NOT NULL DEFAULT '';
UPDATE runtime_instances SET local_instance_id=id,worker_endpoint_key=endpoint_key,endpoint_key=char(0)||rowid;
UPDATE runtime_instances SET endpoint_key='worker/local-worker/runtime/'||id;
CREATE UNIQUE INDEX worker_runtime_local_id ON runtime_instances(worker_id,local_instance_id);
CREATE UNIQUE INDEX worker_runtime_endpoint ON runtime_instances(worker_id,worker_endpoint_key);

ALTER TABLE machines ADD COLUMN worker_id TEXT NOT NULL DEFAULT 'local-worker';
ALTER TABLE runtime_bindings ADD COLUMN worker_id TEXT NOT NULL DEFAULT 'local-worker';
ALTER TABLE operations ADD COLUMN worker_id TEXT NOT NULL DEFAULT 'local-worker';
ALTER TABLE operations ADD COLUMN runtime_instance_id TEXT REFERENCES runtime_instances(id);
ALTER TABLE operations ADD COLUMN runtime_bundle_id TEXT REFERENCES runtime_bundles(id)
    CHECK ((runtime_bundle_id IS NULL) = (runtime_instance_id IS NULL));
UPDATE operations AS o SET (runtime_instance_id,runtime_bundle_id)=(
    SELECT b.runtime_instance_id,b.runtime_bundle_id FROM runtime_bindings b WHERE b.id=o.binding_id
) WHERE o.binding_id IS NOT NULL;
UPDATE operations AS o SET (runtime_instance_id,runtime_bundle_id)=(
    SELECT i.id,i.runtime_bundle_id FROM runtime_instances i
    WHERE i.id=json_extract(o.request_json,'$.runtime_instance_id')
      AND i.runtime_bundle_id=json_extract(o.request_json,'$.runtime_bundle_id')
) WHERE o.binding_id IS NULL AND o.kind='volume';
UPDATE operations AS o SET (runtime_instance_id,runtime_bundle_id)=(
    SELECT p.runtime_instance_id,p.runtime_bundle_id FROM vm_snapshot_provenance p
    WHERE p.snapshot_id=json_extract(o.request_json,'$.snapshot_id')
) WHERE o.kind='vm_snapshot_delete';

-- ALTER TABLE cannot add a populated non-null REFERENCES column in
-- SQLite. These guards enforce reference and pin consistency at the writer.
CREATE TRIGGER worker_identity_immutable BEFORE UPDATE OF id ON workers
WHEN NEW.id IS NOT OLD.id
BEGIN SELECT RAISE(ABORT,'Worker identity is immutable'); END;
CREATE TRIGGER worker_keep_tombstone BEFORE DELETE ON workers
BEGIN SELECT RAISE(ABORT,'Worker records are retained after decommission'); END;

CREATE TRIGGER runtime_worker_insert BEFORE INSERT ON runtime_instances
WHEN NEW.local_instance_id='' OR NEW.worker_endpoint_key=''
    OR NEW.endpoint_key<>'worker/'||NEW.worker_id||'/runtime/'||NEW.id
    OR NOT EXISTS (SELECT 1 FROM workers w WHERE w.id=NEW.worker_id)
BEGIN SELECT RAISE(ABORT,'invalid Worker runtime route'); END;
CREATE TRIGGER runtime_worker_immutable
BEFORE UPDATE OF id,worker_id,local_instance_id,endpoint_key,worker_endpoint_key ON runtime_instances
WHEN NEW.id IS NOT OLD.id OR NEW.worker_id IS NOT OLD.worker_id OR NEW.local_instance_id IS NOT OLD.local_instance_id
    OR NEW.endpoint_key IS NOT OLD.endpoint_key OR NEW.worker_endpoint_key IS NOT OLD.worker_endpoint_key
BEGIN SELECT RAISE(ABORT,'Worker runtime route is immutable'); END;

CREATE TRIGGER machine_worker_insert BEFORE INSERT ON machines
WHEN NOT EXISTS (SELECT 1 FROM workers w WHERE w.id=NEW.worker_id)
BEGIN SELECT RAISE(ABORT,'unknown machine Worker'); END;
CREATE TRIGGER machine_worker_immutable BEFORE UPDATE OF worker_id ON machines
WHEN NEW.worker_id IS NOT OLD.worker_id
BEGIN SELECT RAISE(ABORT,'machine Worker is immutable'); END;
CREATE TRIGGER machine_current_binding_worker BEFORE UPDATE OF current_binding_id ON machines
WHEN NEW.current_binding_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM runtime_bindings b WHERE b.id=NEW.current_binding_id AND b.machine_id=NEW.id AND b.worker_id=NEW.worker_id)
BEGIN SELECT RAISE(ABORT,'current binding differs from machine Worker'); END;

CREATE TRIGGER binding_worker_insert BEFORE INSERT ON runtime_bindings
WHEN NOT EXISTS (SELECT 1 FROM machines m WHERE m.id=NEW.machine_id AND m.worker_id=NEW.worker_id)
    OR (NEW.runtime_instance_id IS NULL AND NEW.worker_id<>'local-worker')
    OR (NEW.runtime_instance_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM runtime_instances i WHERE i.id=NEW.runtime_instance_id AND i.worker_id=NEW.worker_id))
BEGIN SELECT RAISE(ABORT,'binding differs from Worker owner'); END;
CREATE TRIGGER binding_worker_immutable
BEFORE UPDATE OF worker_id,machine_id,runtime_instance_id,runtime_bundle_id ON runtime_bindings
WHEN NEW.worker_id IS NOT OLD.worker_id OR NEW.machine_id IS NOT OLD.machine_id
    OR NEW.runtime_instance_id IS NOT OLD.runtime_instance_id OR NEW.runtime_bundle_id IS NOT OLD.runtime_bundle_id
BEGIN SELECT RAISE(ABORT,'binding Worker route is immutable'); END;

CREATE TRIGGER operation_worker_insert BEFORE INSERT ON operations
WHEN NOT EXISTS (SELECT 1 FROM workers w WHERE w.id=NEW.worker_id)
    OR (NEW.machine_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM machines m WHERE m.id=NEW.machine_id AND m.worker_id=NEW.worker_id))
    OR (NEW.source_machine_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM machines m WHERE m.id=NEW.source_machine_id AND m.worker_id=NEW.worker_id))
    OR (NEW.binding_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM runtime_bindings b WHERE b.id=NEW.binding_id AND b.worker_id=NEW.worker_id AND b.machine_id=NEW.machine_id
        AND b.runtime_instance_id IS NEW.runtime_instance_id AND b.runtime_bundle_id IS NEW.runtime_bundle_id))
    OR (NEW.runtime_instance_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM runtime_instances i WHERE i.id=NEW.runtime_instance_id
        AND i.runtime_bundle_id=NEW.runtime_bundle_id AND i.worker_id=NEW.worker_id))
BEGIN SELECT RAISE(ABORT,'operation differs from Worker owner'); END;
CREATE TRIGGER operation_worker_immutable
BEFORE UPDATE OF worker_id,machine_id,source_machine_id,binding_id,runtime_instance_id,runtime_bundle_id ON operations
WHEN NEW.worker_id IS NOT OLD.worker_id OR NEW.runtime_instance_id IS NOT OLD.runtime_instance_id
    OR NEW.runtime_bundle_id IS NOT OLD.runtime_bundle_id OR NEW.machine_id IS NOT OLD.machine_id
    OR NEW.source_machine_id IS NOT OLD.source_machine_id OR NEW.binding_id IS NOT OLD.binding_id
BEGIN SELECT RAISE(ABORT,'operation Worker route is immutable'); END;

-- A volume/snapshot inherits the immutable owner of its registered route.
CREATE TRIGGER volume_route_insert BEFORE INSERT ON volumes
WHEN NOT EXISTS (SELECT 1 FROM runtime_instances i
    WHERE i.id=NEW.runtime_instance_id AND i.runtime_bundle_id=NEW.runtime_bundle_id)
BEGIN SELECT RAISE(ABORT,'volume differs from Worker route'); END;
CREATE TRIGGER volume_route_immutable BEFORE UPDATE OF runtime_instance_id,runtime_bundle_id ON volumes
WHEN NEW.runtime_instance_id IS NOT OLD.runtime_instance_id OR NEW.runtime_bundle_id IS NOT OLD.runtime_bundle_id
BEGIN SELECT RAISE(ABORT,'volume Worker route is immutable'); END;
CREATE TRIGGER volume_binding_route_insert BEFORE INSERT ON volume_attachments
WHEN NOT EXISTS (
    SELECT 1 FROM volumes v JOIN runtime_bindings b ON b.id=NEW.binding_id AND b.machine_id=NEW.machine_id
    WHERE v.id=NEW.volume_id AND v.runtime_instance_id=b.runtime_instance_id AND v.runtime_bundle_id=b.runtime_bundle_id
)
BEGIN SELECT RAISE(ABORT,'volume attachment differs from Worker route'); END;
CREATE TRIGGER volume_binding_route_immutable BEFORE UPDATE OF volume_id,machine_id,binding_id ON volume_attachments
WHEN NEW.volume_id IS NOT OLD.volume_id OR NEW.machine_id IS NOT OLD.machine_id OR NEW.binding_id IS NOT OLD.binding_id
BEGIN SELECT RAISE(ABORT,'volume attachment Worker route is immutable'); END;
CREATE TRIGGER volume_snapshot_owner_immutable BEFORE UPDATE OF volume_id ON volume_snapshots
WHEN NEW.volume_id IS NOT OLD.volume_id
BEGIN SELECT RAISE(ABORT,'volume snapshot Worker route is immutable'); END;

CREATE TRIGGER statepoint_route_insert BEFORE INSERT ON managed_statepoint_receipts
WHEN NOT EXISTS (
    SELECT 1 FROM runtime_bindings b JOIN operations o ON o.id=NEW.operation_id AND o.binding_id=b.id AND o.worker_id=b.worker_id
    WHERE b.id=NEW.binding_id AND b.runtime_instance_id=NEW.runtime_instance_id
      AND b.runtime_bundle_id=NEW.runtime_bundle_id
)
BEGIN SELECT RAISE(ABORT,'StatePoint differs from Worker route'); END;
CREATE TRIGGER statepoint_route_immutable BEFORE UPDATE ON managed_statepoint_receipts
BEGIN SELECT RAISE(ABORT,'StatePoint receipt is immutable'); END;
