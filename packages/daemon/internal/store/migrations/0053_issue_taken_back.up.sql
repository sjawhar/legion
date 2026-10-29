-- 0053_issue_taken_back.up.sql -- whether the issue has stood in a phase of its own as part of the
-- tree it now roots or belongs to, in this generation or an earlier one (INFRA-225).
--
-- A tree that ran, set back to todo while it lingers after its sign-off or after it closed,
-- starts again whatever Dispatch says it waits on. A root that only stood on the waiting line, left
-- it and came back, and a child of a finished tree handed to Legion as a root of its own without
-- its run, whether or not it ran a phase under that tree, are checked like any other root. By the
-- time re-admission has written them, all three records are the same shape — a tree root at
-- `admitted`, handed over, at a new generation whose tree is its own key — so nothing else in the
-- record tells the first from the other two. The exemption is both halves at once: the issue ran,
-- and the tree coming back is its own. The workflow writes the mark as an issue moves into a phase
-- that carries a run (any but `admitted` and `done`); re-admission keeps it when the issue's own
-- tree comes back and clears it when another tree's work arrives at a root of its own without its
-- run, since what that child ran was its old tree's.
--
-- The mark is stored rather than derived because the judgement is not made in that moment. A
-- returning root whose project has no free slot keeps its place in the waiting line and is judged
-- again by a later pass — a tick, a hold's release, the next boot — long after the run it is
-- judged on, which re-admission has cleared from everything but this column.
--
-- Rows already recorded are marked from what they still show of a run under their present tree:
-- an issue standing in a phase that carries a run, or one whose phase worker has a row of its own
-- (phases), which only a worker that started writes, unless that worker's claim says it ran under
-- another tree. Phase rows outlive a hand-over, so without that test a child an older daemon
-- already handed over as a root of its own, still waiting on the line, would be marked for its old
-- tree's run with no re-admission left to clear it. A claim moves with the work it carries and
-- keeps the tree it last ran under, and a phase row whose claim is gone is read as the present
-- tree's, as before. A row that shows none of this — a root that only waited, a child never
-- started, or one whose run ended before any worker reported — keeps the default and is checked.
-- A child still under the tree it ran in is marked, which changes nothing while it is a child: a
-- child is held by its own links whatever its mark, and its hand-over, if one comes, clears it.
alter table issues add column taken_back boolean not null default false;

update issues set taken_back = true
  where phase not in ('admitted', 'done')
     or exists (select 1 from phases left join claims on claims.token = phases.claim
                where phases.issue = issues.key and phases.role <> 'architect'
                  and coalesce(claims.tree, issues.tree) = issues.tree);
