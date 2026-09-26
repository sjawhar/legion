-- 0015_review_round.up.sql - a review ends when its reviewer completes it, which can come after
-- the review itself, so the reviewer's round keeps the review that decided it until the round
-- ends: its state, its body (handed to the next round's implementer), the head it was written on
-- and its GitHub id. review_seen, kept across rounds, is the newest review id the issue has had: a
-- review no newer than it was written before one already processed.
alter table phases add column decision jsonb;
alter table phases add column review_seen bigint not null default 0;

-- The decision lived on the pull request, where every new head reset it; the round holds it now.
alter table pull_requests drop column review_decision;

-- Every classified push is kept, not only the newest: a push that changed only .legion/ is a fact
-- about two commits, true whenever it is learned, and an approval of a head is carried across such
-- pushes by walking back through them from the current head. The one pending push a row held
-- becomes a list of one.
alter table pull_requests rename column pending_push to pushes;
update pull_requests set pushes = case
  when pushes is null then '[]'::jsonb
  else jsonb_build_array(pushes)
end;
alter table pull_requests alter column pushes set default '[]';
alter table pull_requests alter column pushes set not null;
