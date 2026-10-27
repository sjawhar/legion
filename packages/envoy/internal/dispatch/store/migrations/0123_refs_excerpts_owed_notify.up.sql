-- 0123_refs_excerpts_owed_notify.up.sql
-- Every artifact mention left owed an excerpt (excerpt_version null) announces itself on
-- dispatch_excerpts_owed at commit, which wakes the reference excerpt reconciliation
-- (docs.Service.RunReferenceExcerptReconciliation) in every process through its shared listener,
-- in place of the timer it used to poll on. A trigger rather than a notify in Go, so what the
-- previous release writes during a deploy - its version writes' refs.ClearExcerpts, and
-- rebuild-refs from either build - wakes the loop too. A version write that inserts a mention and
-- cuts its excerpt in the same transaction fires it as well: that transaction already notifies
-- (its event, 0107), and the batch it wakes finds nothing, one probe of the empty
-- refs_excerpt_drift index at most once per two seconds. Postgres folds the repeats of one
-- transaction into one notification. Its own migration, so the transaction holds refs' lock
-- alone, for milliseconds.
create function refs_excerpts_owed_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_excerpts_owed', '');
  return null;
end $$;
create trigger refs_excerpts_owed_notify after insert or update of excerpt_version on refs
  for each row when (new.from_kind = 'artifact' and new.excerpt_version is null)
  execute function refs_excerpts_owed_notify();
