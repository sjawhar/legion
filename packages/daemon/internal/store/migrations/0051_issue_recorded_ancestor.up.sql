-- 0051_issue_recorded_ancestor.up.sql — the recorded ancestor a queued root waits under (INFRA-252).
--
-- A labelled todo issue whose Dispatch parent the daemon does not record (a backlog child of a live
-- tree its architect never released) is recorded as a root of its own. recorded_ancestor is the key
-- of the nearest ancestor on its Dispatch parent chain that the daemon records as a tree's root,
-- read through every other ancestor, when its observation was prepared, or when an event of an
-- issue the daemon does not record, or of a recorded issue a person moved, was; admission keeps
-- such a root out of its waiting line while that root's current tree is live. Empty for every
-- other record, and for every existing row until its next observation.
alter table issues add column recorded_ancestor text not null default '';
