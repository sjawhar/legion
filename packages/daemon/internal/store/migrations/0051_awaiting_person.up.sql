-- 0051_awaiting_person.up.sql — a root waiting only on a person gives up its admission slot.
--
-- Phase `awaiting_person` is a root admitted tree whose design gate is closed on the spec version
-- its architect registered, or whose architect opened a question on it: it holds no slot, and its
-- architect is suspended. What it waits on is recorded beside the phase (awaiting_ask, the ask's
-- id, or awaiting_gate_version, the registered spec version), and awaiting_answered_at is when a
-- person answered it, which puts it at the front of the waiting line. The three are set only in
-- that phase, and exactly one of the ask and the gate version is.
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
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_answered_at is null
  end);

alter table issues add column claimed_by text not null default '';
alter table issues add column claim_seq bigint not null default 0 check (claim_seq >= 0);
