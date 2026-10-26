-- 0051_awaiting_person.up.sql — a root waiting only on a person gives up its admission slot.
--
-- Phase `awaiting_person` is a root admitted tree whose design gate is closed on the spec version
-- its architect registered, or whose architect opened a question on it: it holds no slot, and its
-- architect is suspended. What it waits on is recorded beside the phase (awaiting_ask, the ask's
-- id, or awaiting_gate_version, the registered spec version), awaiting_answered_at is when a
-- person answered it, which puts it at the front of the waiting line, and awaiting_answered_by the
-- act that answered it (the person's act on the ask, a reply on another ask, an approval, a
-- changes request, a newer version), which the task resuming the architect names. awaiting_since_seq and
-- awaiting_since are where the wait began on Dispatch: the issue's event sequence and Dispatch's
-- time of the event that started it (a hand-back, a question opened), or, for a registration, the
-- sequence its read began at, with no time. A person's act whose event sequence is not past that
-- start never answers the wait, and neither does one Dispatch shows with no sequence (recorded
-- before Dispatch kept one). The five are set only in that phase, and exactly one of the ask and
-- the gate version is; either start may be unknown (null).
--
-- wait_fence_seq is the highest Dispatch event sequence of the issue that a wait has started from,
-- kept in every phase and across a resume and a re-admission: a hand-back (ask.handed_back, or a
-- comment.created reply that leaves the turn with the person) or an ask.opened event at or below
-- it is an act a wait already covered, delivered late, and starts no wait. 0 is none.
--
-- gate_fence_seq is the highest issue sequence a register_gate read of Dispatch covered, kept the
-- same way. That read decided the gate alone, so it fences only an event that would start a wait
-- on the gate: a hand-back while the gate is closed. A question opened is fenced by wait_fence_seq
-- alone, since the approval read says nothing about an ordinary ask. 0 is none.
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
alter table issues add column awaiting_answered_by text check (
  awaiting_answered_by in ('ask', 'reply', 'approved', 'changes_requested', 'version'));
alter table issues add column awaiting_since_seq bigint check (awaiting_since_seq > 0);
alter table issues add column awaiting_since timestamptz;
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
      and (awaiting_answered_by is null or awaiting_answered_at is not null)
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_answered_at is null
      and awaiting_answered_by is null and awaiting_since_seq is null and awaiting_since is null
  end);
alter table issues add column wait_fence_seq bigint not null default 0 check (wait_fence_seq >= 0);
alter table issues add column gate_fence_seq bigint not null default 0 check (gate_fence_seq >= 0);

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

-- design_gates.approved_seq and approved_at are where Dispatch recorded the approval the gate
-- holds: the review's event sequence on the issue and Dispatch's time of it (either null when the
-- read that carried it gave none). They clear wherever the approval clears (a changes request),
-- and a newer version keeps them with the approval it closes, so the gate records the mark of its
-- newest applied review in one of two places — here while it records an approval, and in
-- told_changes_* while a changes request stands. A review of the version recorded here whose own
-- mark is not past the recorded one is one the person has already followed with another, and
-- changes no gate state.
alter table design_gates add column approved_seq bigint check (approved_seq > 0);
alter table design_gates add column approved_at timestamptz;
alter table design_gates add constraint design_gates_approved_mark_shape check (
  approved_version is not null or (approved_seq is null and approved_at is null));

-- awaiting_person_acts records each act a person took on an ask of a root — an answer or
-- resolution that closed it, or a reply that left the turn with the agent — by its event's
-- sequence on the issue, with Dispatch's time of it (null when the event carried none), the ask,
-- and whether it answers a wait on the root's design gate as well as one on that ask. Dispatch's
-- intake retries a delivery, so the event that starts a wait (a question opened, a hand-back) can
-- reach the daemon after a person's act that followed it: a wait whose start an act recorded here
-- follows starts none, since what it would wait on has already happened. The rows are the issue's
-- and go when it leaves the workflow.
create table awaiting_person_acts (
  issue text not null references issues (key) on delete cascade,
  seq bigint not null check (seq > 0),
  ask text not null check (ask <> ''),
  at timestamptz,
  answers_gate boolean not null,
  primary key (issue, seq));
