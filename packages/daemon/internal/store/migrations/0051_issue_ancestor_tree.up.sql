-- 0051_issue_ancestor_tree.up.sql — the live tree a queued root waits under (INFRA-252).
--
-- A labelled todo issue whose Dispatch parent the daemon does not record (a backlog child of a live
-- tree its architect never released) is recorded as a root of its own. ancestor_tree is the recorded
-- tree of its nearest recorded Dispatch ancestor, read when its observation was prepared; admission
-- keeps such a root out of its waiting line while that tree is live. Empty for every other record,
-- and for every existing row until its next observation.
alter table issues add column ancestor_tree text not null default '';
