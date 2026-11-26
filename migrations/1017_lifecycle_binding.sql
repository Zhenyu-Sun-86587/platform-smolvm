CREATE UNIQUE INDEX lifecycle_delivery_revision ON lifecycle_deliveries(binding_id,desired_revision);
ALTER TABLE lifecycle_deliveries ADD COLUMN previous_revision INTEGER NOT NULL DEFAULT 0 CHECK(previous_revision>=0);
UPDATE lifecycle_deliveries SET previous_revision=desired_revision-1;

-- An application receipt and an event cursor both belong to the Worker instance
-- that produced them. Older registries kept these projections on the machine.
DELETE FROM lifecycle_outbox_cursors
 WHERE NOT EXISTS (
    SELECT 1 FROM machines m JOIN lifecycle_deliveries d ON d.binding_id=m.current_binding_id
     WHERE m.id=lifecycle_outbox_cursors.machine_id AND d.machine_id=m.id AND d.delivered_at IS NOT NULL
 );
UPDATE machine_lifecycle_policies AS p
   SET applied_revision=0,applied_json=NULL,worker_status=NULL,last_error=NULL
 WHERE applied_revision>0 AND NOT EXISTS (
    SELECT 1 FROM machines m JOIN lifecycle_deliveries d ON d.binding_id=m.current_binding_id
     WHERE m.id=p.machine_id AND d.machine_id=m.id AND d.desired_revision=p.applied_revision AND d.delivered_at IS NOT NULL
 );
UPDATE lifecycle_lease_intents AS l
   SET state='rejected',secret=x'',receipt_json='{"code":"E_STALE_EXECUTION","message":"lease binding was replaced"}'
 WHERE state='pending' AND NOT EXISTS (
    SELECT 1 FROM machines m WHERE m.id=l.machine_id AND m.current_binding_id=l.binding_id AND m.deleted_at IS NULL
 );

CREATE TRIGGER lifecycle_binding_replaced AFTER UPDATE OF current_binding_id ON machines
WHEN OLD.current_binding_id IS NOT NEW.current_binding_id
BEGIN
    UPDATE machine_lifecycle_policies
       SET applied_revision=0,applied_json=NULL,worker_status=NULL,last_error=NULL
     WHERE machine_id=NEW.id;
    DELETE FROM lifecycle_outbox_cursors WHERE machine_id=NEW.id;
    UPDATE lifecycle_lease_intents
       SET state='rejected',secret=x'',receipt_json='{"code":"E_STALE_EXECUTION","message":"lease binding was replaced"}'
     WHERE machine_id=NEW.id AND state='pending' AND binding_id IS NOT NEW.current_binding_id;
END;
