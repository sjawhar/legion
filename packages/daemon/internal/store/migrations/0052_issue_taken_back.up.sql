-- 0052_issue_taken_back.up.sql -- whether the issue has stood in a phase of its own, in this
-- generation or an earlier one (INFRA-225).
--
-- A tree that ran, set back to todo while it lingers after its sign-off or after it closed,
-- starts again whatever Dispatch says it waits on. A root that only stood on the waiting line, left
-- it and came back, and a child of a finished tree handed to Legion as a root of its own without
-- ever having started a phase, are checked like any other root. By the time re-admission has
-- written them, all three records are the same shape — a tree root at `admitted`, handed over, at a
-- new generation whose tree is its own key — so nothing else in the record tells the first from the
-- other two. The workflow writes the mark as an issue moves into a phase that carries a run (any
-- but `admitted` and `done`), and nothing clears it: an issue that ran once has run.
--
-- The mark is stored rather than derived because the judgement is not made in that moment. A
-- returning root whose project has no free slot keeps its place in the waiting line and is judged
-- again by a later pass — a tick, a hold's release, the next boot — long after the run it is
-- judged on, which re-admission has cleared from everything but this column.
--
-- Rows already recorded are marked from what they still show of a run: an issue standing in a phase
-- that carries a run, or one whose phase worker has a row of its own (phases), which only a worker
-- that started writes. A row that shows neither — a root that only waited, a child never started,
-- or one whose run ended before any worker reported — keeps the default and is checked.
alter table issues add column taken_back boolean not null default false;

update issues set taken_back = true
  where phase not in ('admitted', 'done')
     or exists (select 1 from phases where phases.issue = issues.key and phases.role <> 'architect');
