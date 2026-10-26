-- 0054_design_gate_review_marks.up.sql -- the design gate identifies a review by where Dispatch
-- recorded it, and records the changes request its architect was told of.
--
-- design_gates.approved_seq and approved_at are where Dispatch recorded the approval the gate
-- holds: the review's artifact.* event sequence on the issue and Dispatch's time of it (either
-- null when the read that carried it gave none). They clear wherever the approval clears (a changes
-- request), and a newer version keeps them with the approval it closes.
--
-- design_gates.told_changes_version, told_changes_reason, told_changes_seq and told_changes_at are
-- the changes request the architect has been told of: its version, its reason, and the review's
-- event sequence on the issue and Dispatch's time of it (either null when the read that carried it
-- gave none). A re-read of that same review, by a later register_gate or the gate seed, tells it
-- nothing new, while another review, even at that version with that reason, does. An approval and
-- a newer document version clear the record.
--
-- So the gate records the mark of its newest applied review in one of two places: approved_* while
-- it records an approval, told_changes_* while a changes request stands. A review of that version
-- whose own mark is not past the recorded one is one the person has already followed with another,
-- and changes no gate state.
--
-- No check ties these columns to approved_version or told_changes_version: the daemon keeps that
-- shape itself (record.Postgres.PutGate writes a mark only with its version, and its reader drops
-- one found without it). A daemon of an earlier release running on this schema writes
-- approved_version alone and leaves the new columns as they are, so a cross-column check would
-- refuse its write of a gate whose approval a changes request clears.
alter table design_gates add column approved_seq bigint check (approved_seq > 0);
alter table design_gates add column approved_at timestamptz;

alter table design_gates add column told_changes_version integer check (told_changes_version > 0);
alter table design_gates add column told_changes_reason text not null default '';
alter table design_gates add column told_changes_seq bigint check (told_changes_seq > 0);
alter table design_gates add column told_changes_at timestamptz;
