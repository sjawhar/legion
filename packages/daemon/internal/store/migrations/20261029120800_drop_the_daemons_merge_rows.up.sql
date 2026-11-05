-- 20261029120800_drop_the_daemons_merge_rows.up.sql — the rows of the merge the daemon no longer makes
-- (dispatch://INFRA-714 R2).
--
-- The daemon submits no merge and changes no merge setting, so three outbox kinds have no executor
-- left: ready_merge (its submission), auto_merge_disable (its disarm) and merge_queue_publish (the
-- READY packet sent to a project's merge_queue_role, a key the configuration no longer reads). A
-- row of any of them that an older daemon queued is deleted rather than left to fail its decode
-- for good. The three kinds leave the CHECK in
-- 20261105081500_outbox_kinds_of_both_releases, the one place this branch sets the list, and not
-- here: a rewrite here would run after the release this branch merged on a database that
-- recorded that release's versions, where ADD CONSTRAINT validates the rows the table holds and
-- one pending `issue_bundle` or `issue_release` of its idle release would fail the upgrade. The
-- deletes below need no rewrite of their own, and leaving the three kinds admitted until that
-- migration costs nothing: no daemon of this release queues one.
delete from outbox where kind in ('ready_merge', 'auto_merge_disable', 'merge_queue_publish');

-- READY's merge was one row per issue and head, which that index made unique. Nothing enqueues the
-- kind any more, and the upsert clause that targeted the index is gone with it, so the index would
-- only outlive its reason.
drop index if exists outbox_ready_merge_issue_head_unique;

-- The notices of that merge, which only its rows wrote: merge-refused, merge-stalled and
-- merge-removed told an architect why a submission ended without a merge, and merge-waiting that
-- GitHub was taking long over one. ready-refused went with READY's design gate, which refuses no
-- completion now. A pending row of any of them reports a moment that has passed, and the daemon
-- writes no more, so each is deleted here; the architect's own session reads the pull request on
-- GitHub for anything it still needs.
delete from outbox where kind in ('notice', 'controller_notice')
  and payload->>'kind' in ('merge-refused', 'merge-stalled', 'merge-removed', 'merge-waiting', 'ready-refused');
