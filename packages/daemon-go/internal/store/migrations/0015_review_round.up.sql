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
