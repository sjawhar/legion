-- 0043 moved the replies that landed through POST /api/v1/comments/{id}/reply with a bare
-- reply_to into the ask thread they belong to. A rolling deploy of the image that carries 0043
-- runs the migration once, at the first task to come up, while pre-0043 tasks are still draining
-- requests: a callback reply written by one of those after 0043 commits has exactly the shape
-- 0043 was there to repair, and nothing repairs it. This re-runs the same backfill once, from a
-- later release, when no pre-0043 task can still be serving.
--
-- That is a condition on how this ships, not a fact about the file: it must reach a database in
-- an image built after the one that carried 0043 (production ran 0043 in production-dispatch:36,
-- legion 4aedb01d).
--
-- A fresh database takes both migrations in one deploy and loses nothing: no pre-0043 task ever
-- served it, and it holds no pre-0043 row.
--
-- A database that has served pre-0043 traffic and then takes both at once - roll back to a
-- pre-0043 image, then forward to a main carrying both - does have the window. Its draining
-- pre-0043 tasks write bare reply_to rows after both migrations commit, and neither repairs
-- them. The repair those rows need is a later release re-running this same backfill, the way
-- this one does for 0043.
--
-- What it costs a running server: the statements take RowExclusiveLock on comments, never
-- ACCESS EXCLUSIVE, so traffic to other rows runs throughout, and inserting a reply under a
-- moved row does not block (the foreign key takes FOR KEY SHARE; the update is FOR NO KEY
-- UPDATE). Row locks fall only on the repaired rows and the asks receiving them, and only for
-- the part of the run that holds them: the recursive walk that builds the temp table takes none
-- (6.6 s of the 12.4 s measured on a 406,000-row table). A request that locks one of those rows
-- waits until this transaction commits - at that scale at most ~5.7 s. Two locks reach that
-- far: the `select ... for update` every comment mutation runs (loadCommentForUpdate,
-- api/comment_queries.go) on a repaired row, and the `for update of a` every ask transition
-- runs (loadAskForUpdate, api/asks.go) on an ask this moves a row into, since the ask_id write
-- takes FOR KEY SHARE on each ask it references. Both are taken by the same statement, so both
-- share that ceiling. An ask nothing moves into is untouched (0.08 s), and so is every other
-- row in either table.
--
-- The other direction costs the deploy rather than the request: a request already holding one
-- of these rows when the update reaches it is not delayed at all - this transaction waits on
-- it, which lengthens the advisory-lock window other tasks boot behind (4.54 s against a 3.93 s
-- baseline at 416,000 comments, from one waiting request).
--
-- The statements are 0043's, unchanged - same scoping, same `a.state = 'open'` gate on the turn,
-- same `union` (not `union all`) recursion, which is what terminates on a cyclic reply_to chain
-- the column has no constraint against. Re-running them is safe because the set they select is
-- empty once every row is in its thread: the walk selects only comments whose ask_id is null and
-- whose ancestry reaches an ask, so a second run moves zero rows and the turn update, which reads
-- the same table, touches none. That is the migration's own idempotence, not a property of having
-- run 0043 first.
--
-- This is the done-criterion recorded on LEGION-227: the issue is not closed until a release
-- after the one that introduced 0043 has re-run the backfill.
create temporary table migrated_ask_replies on commit drop as
with recursive ask_thread as (
  select c.id, c.ask_id
  from comments c
  where c.ask_id is not null
  union
  select c.id, t.ask_id
  from comments c join ask_thread t on c.reply_to = t.id
)
select t.id, t.ask_id
from ask_thread t
join comments c on c.id = t.id
where c.ask_id is null;

update comments c
set ask_id = m.ask_id, reply_to = null
from migrated_ask_replies m
where c.id = m.id;

-- A reply that never carried an ask_id never carried a turn either
-- (comments_turn_requires_ask), and only an open ask has a turn to hold. The moved rows take
-- the same turn 0028 gave every other ask reply: a human's reply hands it to the agent, an
-- agent's hands it back.
update comments c
set turn = case when c.author->>'kind' = 'user' then 'agent' else 'human' end
from migrated_ask_replies m
join asks a on a.id = m.ask_id
where c.id = m.id and a.state = 'open';
