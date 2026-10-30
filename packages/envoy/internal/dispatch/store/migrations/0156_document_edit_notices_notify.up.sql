-- Every document edit notice given a due time (due_at not null) announces itself on
-- dispatch_edit_notices at commit, which wakes the edit notice sweep (docs.Service.RunEditNotices)
-- in every process through its shared listener, in place of the 5 s timer it used to poll on; the
-- sweep then reads the earliest due time and waits for it. A trigger rather than a notify in Go,
-- so the notices the previous release opens during a deploy wake this release's sweep too.
--
-- The rows' writers are docs.recordEditNotice's two upserts (openEditNotice sets a due time,
-- holdEditNotice keeps or clears one), called by the settlement, NamedVersion, SnapshotVersion and
-- a person's rebuild, each in a transaction that writes a document version and appends its
-- artifact.version event: that transaction already notifies (dispatch_events, 0107), so this adds
-- no notify-queue lock at commit to a transaction that took none, and Postgres folds the repeats
-- of one transaction into one notification. A held row with no due time announces nothing: the
-- sweep never sends it. The sweep's own claim is a delete, which this trigger never sees. There is
-- no exception among the writers.
--
-- The release before this one never listens on the channel and keeps sweeping every 5 s, so it
-- keeps working beside this one. Its own migration, so the transaction holds
-- document_edit_notices' lock alone, for milliseconds.
create function document_edit_notices_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_edit_notices', '');
  return null;
end $$;
create trigger document_edit_notices_notify after insert or update on document_edit_notices
  for each row when (new.due_at is not null)
  execute function document_edit_notices_notify();
