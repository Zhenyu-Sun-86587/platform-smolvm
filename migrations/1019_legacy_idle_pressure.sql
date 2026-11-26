-- Legacy idle_policy only authorizes time-based Hibernate. Reconcile the
-- corrected policy with its Worker under a new desired revision.
UPDATE machine_lifecycle_policies
   SET desired_revision=desired_revision+1,
       applied_revision=0,
       desired_json=json_set(desired_json,'$.pressure_reclaim','disabled'),
       applied_json=NULL,
       worker_status=NULL,
       last_error=NULL
 WHERE source='legacy'
   AND json_type(overrides_json,'$.pressure_reclaim') IS NULL
   AND coalesce(json_extract(desired_json,'$.pressure_reclaim'),'idle_only')!='disabled';

UPDATE machine_lifecycle_policies
   SET overrides_json=json_set(overrides_json,'$.pressure_reclaim','disabled')
 WHERE source='legacy'
   AND json_type(overrides_json,'$.pressure_reclaim') IS NULL;
