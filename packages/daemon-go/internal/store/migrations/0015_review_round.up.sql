-- 0015_review_round.up.sql - what a review round decided, kept until the round ends. A review ends
-- when its reviewer completes it, which can come after the review itself, so the reviewer's round
-- keeps the review's body, handed to the next round's implementer, and the head the review was
-- made on.
alter table phases add column reason text not null default '';
alter table phases add column reviewed_head text not null default '';

-- A run of consecutive heads, oldest first, each left by a push that changed only .legion/ since
-- the one before, so all carry the same code. Ending at the current head, it lets an approval of
-- any head in it approve the current one. A pull request recorded before this column starts with
-- none, so only an approval of its current head stands until its next push.
alter table pull_requests add column code_heads jsonb not null default '[]';

-- A pull request keeps every push classified before the head it left arrived, not only the
-- newest: a code push followed by a handoff push, both before their heads, must not leave the
-- handoff push alone to speak for the path from the current head. The one pending push a row
-- held becomes a list of one.
alter table pull_requests rename column pending_push to pending_pushes;
update pull_requests set pending_pushes = case
  when pending_pushes is null then '[]'::jsonb
  else jsonb_build_array(pending_pushes)
end;
alter table pull_requests alter column pending_pushes set default '[]';
alter table pull_requests alter column pending_pushes set not null;
