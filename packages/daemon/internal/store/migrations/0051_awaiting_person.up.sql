-- 0051_awaiting_person.up.sql — a root waiting only on a person gives up its admission slot.
--
-- Phase `awaiting_person` is a root admitted tree whose design gate is closed on the spec version
-- its architect registered, or whose architect opened a question on it: it holds no slot, and its
-- architect is suspended. What it waits on is recorded beside the phase (awaiting_ask, the ask's
-- id, or awaiting_gate_version, the registered spec version), and awaiting_answered_at is when a
-- person answered it, which puts it at the front of the waiting line. awaiting_since_seq and
-- awaiting_since are where the wait began on Dispatch: the issue's event sequence and Dispatch's
-- time of the event that started it (a hand-back, a question opened), or, for a registration, the
-- sequence its read began at and the time of the newest review that read showed. A review whose
-- event sequence (or, when Dispatch gave none, whose time) is not past that start never answers
-- the wait. The five are set only in that phase, and exactly one of the ask and the gate version
-- is; either start may be unknown (null).
--
-- wait_fence_seq is the highest Dispatch event sequence of the issue that a wait has started from,
-- or that a register_gate read of Dispatch covered, kept in every phase and across a resume and a
-- re-admission: an ask.handed_back or ask.opened event at or below it is an act a wait or a
-- registration already covered, delivered late, and starts no wait. 0 is none.
--
-- claimed_by is who holds the issue's Dispatch claim (`<kind>:<id>`, '' for nobody or not yet
-- seen), and claim_seq the Dispatch sequence it was seen at: a root another session or a person
-- claimed is never resumed from awaiting_person.
alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'awaiting_person', 'planning', 'implementing', 'testing', 'reviewing', 'accepting',
  'retro', 'merging', 'awaiting_merge', 'done', 'held'));

alter table issues add column awaiting_ask text check (awaiting_ask <> '');
alter table issues add column awaiting_gate_version integer check (awaiting_gate_version > 0);
alter table issues add column awaiting_answered_at timestamptz;
alter table issues add column awaiting_since_seq bigint check (awaiting_since_seq > 0);
alter table issues add column awaiting_since timestamptz;
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_answered_at is null
      and awaiting_since_seq is null and awaiting_since is null
  end);
alter table issues add column wait_fence_seq bigint not null default 0 check (wait_fence_seq >= 0);

alter table issues add column claimed_by text not null default '';
alter table issues add column claim_seq bigint not null default 0 check (claim_seq >= 0);

-- design_gates.told_changes_version, told_changes_reason, told_changes_seq and told_changes_at are
-- the changes request the architect has been told of: its version, its reason, and the review's
-- event sequence on the issue and Dispatch's time of it (either null when the read that carried it
-- gave none). A re-read of that same review, by a later register_gate, the gate seed or the boot
-- catch-up, tells it nothing new, while another review, even at that version with that reason,
-- does. An approval, and a newer document version, clear the record.
alter table design_gates add column told_changes_version integer check (told_changes_version > 0);
alter table design_gates add column told_changes_reason text not null default '';
alter table design_gates add column told_changes_seq bigint check (told_changes_seq > 0);
alter table design_gates add column told_changes_at timestamptz;
alter table design_gates add constraint design_gates_told_changes_shape check (
  told_changes_version is not null
  or (told_changes_reason = '' and told_changes_seq is null and told_changes_at is null));
