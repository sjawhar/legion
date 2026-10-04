-- 0070_agent_artifacts.up.sql
-- LEGION-541: an agent's conversation owns the files and images sent in it, a third artifact owner
-- beside an issue and a project. Every artifact has exactly one owner (artifacts_one_owner):
--   an issue    issue_key and project_key (the issue's project), session_id null;
--   a project   project_key alone;
--   a session   session_id alone, a non-empty Envoy session id, issue_key and project_key null.
-- ref_key, the address every reference stores, is `agent/<session id>/<slug>` for a session's
-- artifact and `<issue or project>/<slug>` as before for any other.
--
-- ref_key was 0009's stored generated column, coalesce(issue_key, project_key) || '/' || slug.
-- Postgres 16 cannot give a generated column a new expression in place (SET EXPRESSION is 17), and
-- dropping and re-adding one rewrites the table, so this does what 0058 did for search: DROP
-- EXPRESSION is a catalog change that keeps every value, and the column becomes a plain one this
-- BEFORE trigger fills on every insert and on every update of what it is built from, or of ref_key
-- itself, so no writer stores another value. For a row with no session_id the trigger's expression
-- is 0009's; artifacts_ref_key_key keeps it unique.
--
-- session_id is null in every existing row and project_key is set in every one, so the check
-- refuses none (the census); adding it reads artifacts, metadata rows that hold no file's bytes,
-- under the ACCESS EXCLUSIVE lock the new column takes for milliseconds anyway. The partial index
-- is an agent's artifact list, newest first, and holds no existing row. It locks artifacts alone.
alter table artifacts add column session_id text;
alter table artifacts alter column project_key drop not null;
alter table artifacts alter column ref_key drop expression;
create function artifacts_ref_key() returns trigger language plpgsql as $$
begin
  new.ref_key := coalesce(new.issue_key, new.project_key, 'agent/' || new.session_id) || '/' || new.slug;
  return new;
end $$;
create trigger artifacts_ref_key before insert or update of issue_key, project_key, session_id, slug, ref_key on artifacts
  for each row execute function artifacts_ref_key();
alter table artifacts add constraint artifacts_one_owner check (
  case
    when session_id is null then project_key is not null
    else session_id <> '' and issue_key is null and project_key is null
  end
);
create index artifacts_session_id on artifacts (session_id, created_at) where session_id is not null;
