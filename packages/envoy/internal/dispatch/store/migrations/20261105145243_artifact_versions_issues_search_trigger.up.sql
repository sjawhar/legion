-- 20261105145243_artifact_versions_issues_search_trigger.up.sql
-- Keeps 20261105145242's issue keyword vector current: a new version of an issue's primary
-- document re-indexes that issue, so a word a spec gains is found from the write that adds it.
-- 0140's shape, which enqueues the issue's embedding from the same event, for the same reason the
-- issues trigger cannot do this itself: 0057's trigger reads an update of the issue's key or its
-- title, and a spec edit writes neither.
--
-- A function and a trigger and nothing else, as 20261101182454 is, and its own migration for that
-- migration's reason: artifact_versions is 5.2 GiB, far above the largest table a migration may
-- touch (pgmigrate.CensusTableLimit), and a migration made only of statements that change the
-- catalog and read no row is exempt from that limit (pgmigrate.tableSizeExemption). The re-index
-- of the rows already stored is an update, which is not such a statement, so it ships in
-- 20261105145242 instead and this migration's transaction holds artifact_versions' SHARE ROW
-- EXCLUSIVE alone: a version write waits for the commit, a read does not.
--
-- It is the second of the two, so that a version an older build writes between the two commits
-- meets no function the database does not have yet; what it costs is that such a version does not
-- re-index its issue, which the next write of that document does.
--
-- `when (new.markdown is not null)` for 20261101182454's reason: a version with no markdown is an
-- image or a file upload, never a spec, and the guard keeps the trigger off every such write.
--
-- The update takes the issue's row. Every writer of an issue document's version already holds it:
-- docs/service.go locks the document's owner, which for an issue document is the issue row,
-- `for no key update` before it writes the version, so the trigger upgrades a lock its own
-- transaction holds and waits for nothing new. A project document's artifact names no issue, so
-- the join finds none and the trigger writes nothing.
--
-- Writing search fires 20261101182522's issues_search_term_queue, which is what should happen: the
-- issue's vector changed, and the related-terms refresh must count it again. It fires no other
-- trigger of issues, since 0057's and 0073's each read an update of the key or the title.
create function artifact_versions_issues_search() returns trigger language plpgsql as $$
declare
  v_key text;
begin
  select i.key into v_key
  from artifacts a join issues i on i.key = a.issue_key
  where a.id = new.artifact_id and a.is_primary;
  if found then
    update issues set search = issue_search_vector(key, title, issue_embedding_spec(key))
     where key = v_key;
  end if;
  return null;
end $$;
create trigger artifact_versions_issues_search after insert on artifact_versions
  for each row when (new.markdown is not null) execute function artifact_versions_issues_search();
