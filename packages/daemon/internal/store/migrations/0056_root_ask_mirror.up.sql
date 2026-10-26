-- 0056_root_ask_mirror.up.sql -- the daemon mirrors Dispatch's state of every ask of a root, the
-- approval request a design gate's registration found open, and each issue's Dispatch claim.
-- Nothing in the daemon reads any of it to decide anything yet: the tables are kept current so a
-- later release can decide from them, and every column is additive, so a daemon of an earlier
-- release runs on this schema unchanged (it never names a new column, and every new column has a
-- default or is nullable).
--
-- root_asks mirrors Dispatch's state of each ask of a root. It is level data, not a queue of
-- events, and each part of it has a fence of its own, the issue's event sequence of the newest
-- event that gave that part, so an event older than the part changes none of it and the same
-- events in any order leave the same row:
--   open, fenced by seq: whether the ask is open, from the newest event or read that said so
--     (0 while none has: a row only a turn reached).
--   waiting_on, fenced by waiting_on_seq: whose turn the ask was last on. A close names no turn,
--     so a closed ask keeps the turn it last had, and an ask Dispatch's settlement restores
--     reopens on it; an open ask no event gave a turn is with the person, as an ask opens.
--   closed_by, fenced by closed_seq: the newest close of the ask (who closed it: a person, a
--     session of the tree, Dispatch's settlement, anyone else), kept when the ask reopens. A reply
--     on a closed ask says the ask is closed but not who closed it, so a closed row whose close has
--     not arrived yet has none.
-- kind (null until an event names it, and null for a kind this daemon does not know), by_tree (a
-- session of the root's tree opened the ask) and gate_request (a registration bound the ask as
-- its design gate's approval request) are facts of the ask, kept whatever order its events arrive
-- in. person_answer_seq is the newest answer of the ask by a person, and person_seq the newest
-- other act of a person that would answer a root waiting on that person: a resolution, or a reply
-- that does not leave the ask with the person (a comment on a closed ask included). A person's
-- answer would answer such a root unless the ask is a gate's request, whose answer is a review
-- that answers through its own artifact event; the row keeps both, so binding the request before
-- or after its answer arrives decides the same. The rows are Dispatch's and outlive the root's
-- runs.
create table root_asks (
  issue text not null references issues (key) on delete cascade,
  ask text not null check (ask <> ''),
  kind text check (kind in ('question', 'approval')),
  open boolean not null,
  seq bigint not null check (seq >= 0),
  waiting_on text check (waiting_on in ('human', 'agent')),
  waiting_on_seq bigint not null default 0 check (waiting_on_seq >= 0),
  closed_by text check (closed_by in ('person', 'tree', 'settlement', 'other')),
  closed_seq bigint not null default 0 check (closed_seq >= 0),
  by_tree boolean not null default false,
  gate_request boolean not null default false,
  person_seq bigint not null default 0 check (person_seq >= 0),
  person_answer_seq bigint not null default 0 check (person_answer_seq >= 0),
  primary key (issue, ask),
  check (seq > 0 or not open),
  check ((waiting_on is null) = (waiting_on_seq = 0)),
  check ((closed_by is null) = (closed_seq = 0)));

-- design_gates.request_ask is the approval request a registration read open on a document
-- (the ask Dispatch's approval read names while a request is awaiting), and request_artifact that
-- document; both null when none was ever read open. They are written together, and the request is
-- the gate's only while request_artifact is the gate's artifact_id: an earlier release, which
-- names neither column, moves artifact_id alone, and the request it leaves behind is then no
-- request of the gate's document. A registration that finds none open keeps the request recorded
-- for the same document; any other registration rebinds it, to its document's open request or to
-- none. Its root_asks row says whether the request is still with the person.
alter table design_gates add column request_ask text check (request_ask <> '');
alter table design_gates add column request_artifact text check (request_artifact <> '');
alter table design_gates add constraint design_gates_request_check check ((request_ask is null) = (request_artifact is null));

-- dispatch_issues.claimed_by is who Dispatch shows holding the issue's claim, `<kind>:<id>` ('' for
-- nobody), and claim_seq the Dispatch sequence of the observation that showed it (0 for none yet).
-- They carry a fence of their own, apart from dispatch_seq's: an issue event, a claim event, the
-- boot listing or the periodic re-read writes them only when its sequence is newer than claim_seq,
-- so a claim event, which says nothing of the status or the blockers, never moves those, and an
-- issue event older than a claim event already applied never rolls the claim back.
alter table dispatch_issues add column claimed_by text not null default '';
alter table dispatch_issues add column claim_seq bigint not null default 0 check (claim_seq >= 0);
