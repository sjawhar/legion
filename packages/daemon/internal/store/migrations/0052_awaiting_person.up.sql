-- 0052_awaiting_person.up.sql — a root waiting only on a person gives up its admission slot.
--
-- Phase `awaiting_person` is a root admitted tree whose design gate is closed on the spec version
-- its architect registered, or whose architect opened a question on it: it holds no slot, and its
-- architect is suspended. What it waits on is recorded beside the phase (awaiting_ask, the ask's
-- id, or awaiting_gate_version, the registered spec version on the document awaiting_artifact),
-- awaiting_answered_at is when the wait was answered, which puts it at the front of the waiting
-- line, awaiting_answered_by the act that answered it (the person's act on the ask, settlement's
-- retraction of that ask, a session of the tree closing it, a person's act on another ask, a
-- session handing an awaited question to the agent's turn, an approval, a changes request, a
-- newer version) and awaiting_answered_detail that act's own detail (the ask acted on, or the
-- version and reason of the review), which the task resuming the architect names.
-- awaiting_since_seq and awaiting_since are where the wait began on Dispatch: the earliest event
-- sequence of the hand-backs the architect made since it was last given work (issues.declared_seq),
-- with no time. A person's act whose event sequence is not past that start never answers the
-- wait, and neither does one Dispatch shows with no sequence (recorded before Dispatch kept one).
-- The five are set only in that phase, and exactly one of the ask and the gate version is; either
-- start may be unknown (null).
--
-- handout_seq is the highest Dispatch sequence of the issue the record held when the daemon last
-- gave the root's architect work (its admission, its re-admission, or a resume), kept in every
-- phase and never lowered. declared_seq is the lowest sequence of a hand-back since then: a tree
-- session's question opened, a reply of a tree session leaving an ask with the person, an
-- approval request handed back, or a registration of a closed gate whose request waits on the
-- person; null when the architect has handed nothing back since its last handout. An event at or
-- below handout_seq declares nothing: the work it belonged to was already handed out. A root
-- waits on a person only once it declared (the workflow's ReconcileAwaiting).
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
alter table issues add column awaiting_artifact text check (awaiting_artifact <> '');
alter table issues add column awaiting_answered_at timestamptz;
alter table issues add column awaiting_answered_by text check (
  awaiting_answered_by in ('ask', 'retracted', 'closed', 'reply', 'turn', 'approved', 'changes_requested', 'version'));
alter table issues add column awaiting_answered_detail text check (awaiting_answered_detail <> '');
alter table issues add column awaiting_since_seq bigint check (awaiting_since_seq > 0);
alter table issues add column awaiting_since timestamptz;
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
      and (awaiting_gate_version is null or awaiting_artifact is not null)
      and (awaiting_answered_by is null or awaiting_answered_at is not null)
      and (awaiting_answered_detail is null or awaiting_answered_by is not null)
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_artifact is null
      and awaiting_answered_at is null and awaiting_answered_by is null and awaiting_answered_detail is null
      and awaiting_since_seq is null and awaiting_since is null
  end);
alter table issues add column handout_seq bigint not null default 0 check (handout_seq >= 0);
alter table issues add column declared_seq bigint check (declared_seq > 0);

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

-- design_gates.request_ask is the approval request register_gate read open on the document (the
-- ask Dispatch's approval read names while a request is awaiting), null when none was open. Its
-- root_asks row says whether the request is still with the person.
alter table design_gates add column request_ask text check (request_ask <> '');

-- root_asks mirrors Dispatch's state of each ask of a root, last writer wins by the issue's event
-- sequence: it is level data, not a queue of events. seq is the newest event or read applied to
-- the row's state (open, waiting_on, closed_seq, closed_by), and an older one changes none of it.
-- kind (null until an event names it) and by_tree (a session of the root's tree opened the ask)
-- are facts of the ask's opening, kept whatever order its events arrive in. person_seq is the
-- newest act of a person on the ask that answers it — an answer or resolution of a question, a
-- reply that left an open ask with the agent — and closed_seq the event that closed it, with who
-- closed it (closed_by: a person, a session of the tree, Dispatch's settlement, anyone else).
-- waiting_on is whose turn an open ask waits on, null once it is closed. The rows are Dispatch's
-- and outlive the root's runs.
create table root_asks (
  issue text not null references issues (key) on delete cascade,
  ask text not null check (ask <> ''),
  kind text check (kind in ('question', 'approval')),
  open boolean not null,
  waiting_on text check (waiting_on in ('human', 'agent')),
  seq bigint not null check (seq > 0),
  by_tree boolean not null default false,
  person_seq bigint not null default 0 check (person_seq >= 0),
  closed_seq bigint not null default 0 check (closed_seq >= 0),
  closed_by text check (closed_by in ('person', 'tree', 'settlement', 'other')),
  primary key (issue, ask),
  check (open = (waiting_on is not null)),
  check (open = (closed_by is null)));
