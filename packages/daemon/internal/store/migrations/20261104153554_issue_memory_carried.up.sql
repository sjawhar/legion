-- 20261104153554_issue_memory_carried.up.sql — the hand-over's own answer about a root's memory.
--
-- A child its root hands over as a tree of its own keeps the group its work has been running
-- under, and the hand-over is where that is settled (record.Store.CarryMemory): a tree the daemon
-- had opened hands its answer over — its draw verbatim where it had one, and no draw at all where
-- it had none — and a tree nothing ever opened hands nothing over, so the child is drawn at its own
-- first open like any other new tree.
--
-- memory_carried is the hand-over's mark: record.Store.CarryMemory sets it on every carry, the
-- carry that copies a draw included, and record.Store.DrawMemory's refusal is what reads it, so the
-- draw writes nothing for a root whose hand-over settled it. It is also the only thing on the row
-- that holds the answer where that answer is no draw. Without it the draw would have to read the
-- old tree's lifecycle rows itself, at the later moment the child is first opened, and the answer
-- would turn on which of the two roots is promoted first.
--
-- Apart from the backfill below, the column is written by record.Store.CarryMemory alone and read
-- by record.Store.DrawMemory alone. It is in issueColumns, scanIssue, PutIssue and record.Issue
-- nowhere: the two statements name it themselves, and no reader of an issue has anything to do
-- with it.
alter table issues add column memory_carried boolean not null default false;

-- The backfill settles the hand-overs the release before this one recorded. handed_over_from has
-- been on issues since 0059, and every re-admission that makes a child a tree of its own writes
-- it, so an upgrade to this release finds roots handed over out of trees that ran with no memory,
-- the setting having not existed while they ran. Such a child has no tree_lifecycles row of its
-- own yet, because it waits for a slot or a blocker, and no carry ever ran for it: left false, its
-- first open would draw it under whatever mode is in force by then, and work that has been running
-- without memory would take the overlays and the memory instructions at its next launch.
--
-- The rule below is the one record.Store.CarryMemory runs at a hand-over made from now on. The old
-- tree's tree_lifecycles row is the fact that the tree was opened at all, and a tree with no row
-- hands nothing over, so that child keeps its own first draw. The old tree holds no draw to copy,
-- since memory is younger than every one of these hand-overs, so the carry is the mark alone. A
-- root that already carries a draw keeps it, as CarryMemory's own condition does.
-- tree_lifecycles.project is the normalized runtime token, which 0033 writes with this same
-- regexp_replace, where issues.project is the Dispatch key, so the key is normalized to match it.
-- No ordering applies: these hand-overs are already done.
update issues set memory_carried = true
where handed_over_from <> '' and memory_drawn_at is null
  and exists (select 1 from tree_lifecycles l
              where l.project = regexp_replace(lower(issues.project), '[^a-z0-9]', '', 'g')
                and l.tree = issues.handed_over_from);
