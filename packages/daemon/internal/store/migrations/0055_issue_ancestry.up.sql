-- 0055_issue_ancestry.up.sql — the Dispatch chain a queued root waits under, and the tree a root
-- left (INFRA-252).
--
-- A labelled todo issue under a Dispatch parent that is not recorded, or is recorded as a root of
-- its own (a backlog child of a live tree its architect never released, or a root parked under
-- one), is recorded as a root of its own. ancestry is the keys of its Dispatch parent chain, from
-- its parent upward to the top, stopping only at a repeated key or back at the issue, read when its
-- observation was prepared, or when an event of an issue on that chain (one the daemon does not
-- record, or a recorded one a person moved) was; admission keeps such a root out of its waiting
-- line while any live root is on that chain, exactly when release_children from that root would
-- reach it. Empty for every other record, and for every existing row until its next observation.
--
-- handed_over_from is the key of the tree an issue left when re-admission made it a root of its
-- own: a child its finished tree handed over, carrying its run or not, or an orphan a person
-- admitted. Such a root stays its own tree: no admission sweep and no release_children adopts it,
-- into the tree it left or any other, and no ancestry hold holds it. Empty for every other record,
-- and for every existing row.
alter table issues add column ancestry text[] not null default '{}';
alter table issues add column handed_over_from text not null default '';
