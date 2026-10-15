-- LEGION-761: Delivery's settings name a list of production jobs, so a deploy repository that
-- renames its job keeps the old name beside the new one and both its history and its new runs count;
-- and an optional apply step, the step whose success means a successful production job applied
-- (a run a newer run supersedes can finish its production job as success having applied nothing).
-- production_job_names starts as the stored name, one entry. During the one rolling-release
-- compatibility window, an older task still serving reads and writes production_job_name alone;
-- its bare write deliberately collapses production_job_names to that one name, and the trigger
-- keeps production_job_name at the list's first name on every write. After the rollout, the
-- post-deploy settings PUT to this release repairs the list by writing every intended production
-- job name. This release never names production_job_name; a later release drops it with the trigger.
alter table delivery_settings
  add column production_job_names text[],
  add column production_apply_step_name text;

update delivery_settings set production_job_names = array[production_job_name];

alter table delivery_settings
  alter column production_job_names set not null,
  add constraint delivery_settings_production_job_names_check
    check (cardinality(production_job_names) >= 1 and array_position(production_job_names, null) is null);

create function delivery_settings_production_job_names() returns trigger language plpgsql as $$
begin
  if new.production_job_names is null then
    new.production_job_names := array[new.production_job_name];
  elsif tg_op = 'UPDATE'
        and new.production_job_names is not distinct from old.production_job_names
        and new.production_job_name is distinct from old.production_job_name then
    new.production_job_names := array[new.production_job_name];
  end if;
  new.production_job_name := new.production_job_names[1];
  return new;
end $$;

create trigger delivery_settings_production_job_names before insert or update on delivery_settings
  for each row execute function delivery_settings_production_job_names();
