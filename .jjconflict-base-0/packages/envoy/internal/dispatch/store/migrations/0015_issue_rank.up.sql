alter table issues add column rank text;

with ordered as (
  select key, lpad(row_number() over (partition by project_key order by created_at, key)::text, 20, '0') as rank
  from issues
)
update issues set rank = ordered.rank from ordered where issues.key = ordered.key;

alter table issues alter column rank set not null;
create index issues_project_rank on issues (project_key, rank);
