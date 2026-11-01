-- 0021_project_events.up.sql
alter table projects add column last_seq integer not null default 0;
alter table events add column project_key text references projects(key);
alter table events drop constraint events_one_owner;
alter table events add constraint events_one_owner check (
  num_nonnulls(issue_key, artifact_id, project_key) = 1
);
alter table events add constraint events_project_key_seq_key unique (project_key, seq);
