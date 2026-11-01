alter table messages alter column issue_key drop not null;
alter table messages add constraint messages_issue_or_target check (issue_key is not null or target is not null);

alter table events drop constraint events_one_owner;
alter table events add constraint events_one_owner check (
  num_nonnulls(issue_key, artifact_id, project_key) <= 1
);
