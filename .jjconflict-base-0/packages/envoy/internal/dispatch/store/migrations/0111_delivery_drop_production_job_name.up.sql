-- LEGION-761: 0109 kept delivery_settings.production_job_name through one rolling-release window,
-- for a task of the release before it, which read and wrote that column alone; its trigger set the
-- column to the first of production_job_names on every write. Every release from legion-envoy
-- v13.4.1, the one that added 0109, reads and writes production_job_names alone, so the trigger,
-- its function and the column go. The drop loses no value: the trigger kept the column equal to
-- production_job_names[1], which stays (the census counts any row where it did not). The trigger
-- goes before the function it runs, and both before the column the function names. Holds
-- delivery_settings, one row, ACCESS EXCLUSIVE for milliseconds: dropping a column rewrites no row.
drop trigger delivery_settings_production_job_names on delivery_settings;

drop function delivery_settings_production_job_names();

alter table delivery_settings drop column production_job_name;
