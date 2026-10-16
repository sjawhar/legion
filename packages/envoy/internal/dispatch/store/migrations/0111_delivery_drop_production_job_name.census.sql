-- 0111 refuses no row, and the column it drops holds no value the remaining columns lack: since
-- 0109_delivery_production_job_names, which shipped in an earlier release (legion-envoy v13.4.1),
-- its trigger has set production_job_name to production_job_names[1] on every write. This counts
-- the rows where the two differ, whose dropped value would be lost; the trigger keeps it at 0. The
-- table and the column come from 0079_delivery_settings, which shipped in an earlier release too.
select count(*)
from delivery_settings
where production_job_name is distinct from production_job_names[1];
