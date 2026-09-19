alter table doc_updates add column content_changed boolean not null default true;
alter table artifact_versions add column doc_update_version bigint not null default 0;

update artifact_versions versions
set doc_update_version = coalesce((
  select max(updates.version) from doc_updates updates where updates.artifact_id = versions.artifact_id
), 0)
where doc_update_version = 0;
