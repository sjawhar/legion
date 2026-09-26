-- 0015_phase_reason.up.sql - what a recorded decision said. A review ends when its reviewer
-- completes it, which can come after the review itself, so the review's body is kept on the
-- reviewer's round until the round ends and handed to the next round's implementer then.
alter table phases add column reason text not null default '';
