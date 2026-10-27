-- 0059_issue_ancestry.up.sql — the Dispatch chain a queued root waits under, and the tree a root
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
-- into the tree it left or any other, and no ancestry hold holds it. Empty for every other record.
--
-- Rows an older daemon already made roots of their own are marked from what they still show of the
-- tree they left, since the older daemon adopted nothing and the first admission sweep after this
-- migration would otherwise take them back: a root of its own (key = tree) that the outbox handed
-- over, whose `handover:<KEY>:<row id>` event processed_events keeps under source 'outbox' (never
-- pruned), or whose phase worker's claim still names another tree, since a claim keeps the tree it
-- last ran under (0053). The value is that claim's tree, else the issue's parent; only a non-empty
-- value matters. No status is tested: a handed-over root that went to backlog and came back stays
-- its own tree. A row that shows neither, an orphan a person admitted that never ran a worker and
-- was never handed over by the outbox, keeps the default; the pull request's deploy note lists the
-- query that finds such rows before the upgrade.
alter table issues add column ancestry text[] not null default '{}';
alter table issues add column handed_over_from text not null default '';

update issues set handed_over_from = coalesce(
    (select min(claims.tree) from claims where claims.issue = issues.key and claims.tree <> issues.key),
    issues.parent,
    issues.key)
  where issues.key = issues.tree
    and (exists (select 1 from processed_events
                 where processed_events.source = 'outbox'
                   and split_part(processed_events.event_id, ':', 1) = 'handover'
                   and split_part(processed_events.event_id, ':', 2) = issues.key)
      or exists (select 1 from claims where claims.issue = issues.key and claims.tree <> issues.key));
