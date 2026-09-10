-- 0009_project_artifacts.up.sql
do $$
declare bad record;
begin
  select from_kind, from_id, to_id into bad from refs
  where to_kind = 'artifact' and to_id !~ '^[A-Z][A-Z0-9]{1,9}(-[0-9]+)?/.+$' limit 1;
  if found then
    raise exception 'refs row (%, %) has malformed artifact target %', bad.from_kind, bad.from_id, bad.to_id;
  end if;
  select a.id into bad from artifacts a left join issues i on i.key = a.issue_key where i.key is null limit 1;
  if found then
    raise exception 'artifact % has no issue', bad.id;
  end if;
end $$;

alter table artifacts add column project_key text references projects(key);
update artifacts a set project_key = i.project_key from issues i where i.key = a.issue_key;
alter table artifacts alter column project_key set not null;
alter table artifacts alter column issue_key drop not null;
alter table artifacts drop constraint artifacts_issue_key_slug_key;
alter table artifacts add column ref_key text generated always as (coalesce(issue_key, project_key) || '/' || slug) stored;
alter table artifacts add constraint artifacts_ref_key_key unique (ref_key);
alter table artifacts add column last_seq integer not null default 0;
alter table artifacts add constraint artifacts_primary_has_issue check (not is_primary or issue_key is not null);

alter table asks alter column issue_key drop not null;
alter table asks add column artifact_id uuid references artifacts(id);
alter table asks add constraint asks_one_owner check ((issue_key is null) <> (artifact_id is null));
create index asks_open_artifact on asks (artifact_id) where state = 'open' and artifact_id is not null;

alter table comments alter column issue_key drop not null;
alter table comments add column artifact_id uuid references artifacts(id);
alter table comments add constraint comments_one_owner check ((issue_key is null) <> (artifact_id is null));

alter table events alter column issue_key drop not null;
alter table events add column artifact_id uuid references artifacts(id);
alter table events add constraint events_one_owner check ((issue_key is null) <> (artifact_id is null));
alter table events add constraint events_artifact_id_seq_key unique (artifact_id, seq);
