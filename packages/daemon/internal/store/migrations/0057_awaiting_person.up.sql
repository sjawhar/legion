-- 0057_awaiting_person.up.sql -- a root whose architect asked a person a question, and now waits
-- only on that person, gives up its admission slot.
--
-- Phase `awaiting_person` is a root admitted tree that waits only on a person: it holds no slot,
-- and its architect is suspended. What it waits on is recorded beside the phase: awaiting_ask, the
-- ask's id, or awaiting_gate_version, a registered spec version on the document awaiting_artifact
-- (this release records only question waits; the gate shape is kept for the release that waits on
-- a design gate). awaiting_answered_at is when the wait was answered, which puts the root at the
-- front of the waiting line, awaiting_answered_by the act that answered it (the person's act on
-- the awaited ask, settlement's retraction of it, a close of it by anyone who is not a person, a
-- person's act on another ask, a session handing the last question still with the person to the
-- agent's turn, an approval, a changes request, a newer version) and awaiting_answered_detail that
-- act's own detail (the ask acted on, or the version and reason of the review), which the task
-- resuming the architect names. awaiting_since_seq and awaiting_since are where the wait began on
-- Dispatch: the newest event sequence of the hand-backs the architect made since it was last given
-- work (issues.declared_seq), with no time. A person's act whose event sequence is not past that
-- start never answers the wait. The columns are set only in that phase, and exactly one of the ask
-- and the gate version is; either start may be unknown (null).
--
-- handout_seq is the highest Dispatch sequence of the issue the record held when the daemon last
-- gave the root's architect work (its admission, its re-admission, a resume, a design-changes
-- notice), kept in every phase and never lowered. declared_seq is the newest sequence of a
-- hand-back since then: a tree session's question opened, or a reply of a tree session leaving
-- such a question with the person; null when the architect has handed nothing back since its last
-- handout, or when nothing it handed back is still with the person. An event at or below
-- handout_seq declares nothing: the work it belonged to was already handed out. A root waits on a
-- person only once it declared (the workflow's ReconcileAwaiting).
--
-- Every issue recorded before this migration takes handout_seq from the highest sequence the
-- record already holds of it: its last Dispatch observation, the claim its Dispatch mirror row
-- shows, its mirrored asks and its design gate's review marks. So no event the daemon applied
-- before this release, and no event of the same work redelivered after it, declares a hand-back:
-- a root admitted at the deploy waits on a person only after its architect hands something back
-- again.
--
-- A daemon of an earlier release cannot run on this schema while a root is in awaiting_person: it
-- does not know the phase, and a write of such a row that leaves the phase without clearing the
-- columns fails the shape check below.
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

update issues i set handout_seq = greatest(i.last_dispatch_seq,
  coalesce((select d.claim_seq from dispatch_issues d where d.key = i.key), 0),
  coalesce((select max(a.seq) from root_asks a where a.issue = i.key), 0),
  coalesce((select greatest(coalesce(g.approved_seq, 0), coalesce(g.told_changes_seq, 0)) from design_gates g where g.issue = i.key), 0));
