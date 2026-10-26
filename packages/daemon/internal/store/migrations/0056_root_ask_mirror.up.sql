-- 0056_root_ask_mirror.up.sql -- the daemon mirrors Dispatch's state of every ask of a root, the
-- approval request a design gate's registration found open, and each issue's Dispatch claim.
-- Nothing in the daemon reads any of it to decide anything yet: the tables are kept current so a
-- later release can decide from them, and every column is additive, so a daemon of an earlier
-- release runs on this schema unchanged (it never names a new column, and every new column has a
-- default or is nullable).
--
-- root_asks mirrors Dispatch's state of each ask of a root, last writer wins by the issue's event
-- sequence: it is level data, not a queue of events. seq is the newest event or read applied to
-- the row's state (open, waiting_on, closed_seq, closed_by), and an older one changes none of it.
-- kind (null until an event names it, and null for a kind this daemon does not know) and by_tree
-- (a session of the root's tree opened the ask) are facts of the ask's opening, kept whatever
-- order the ask's events arrive in. person_seq is the newest act of a person on the ask that would
-- answer a root waiting on that person: an answer or a resolution of the ask (but an answer of the
-- approval request a gate of the root names, which is a review and answers through its own
-- artifact event), or any reply that does not leave the ask with the person, a comment on a closed
-- ask included. closed_seq is the event that closed it, with who closed it (closed_by: a person, a
-- session of the tree, Dispatch's settlement, anyone else). waiting_on is whose turn the ask was
-- last on; a closed ask keeps the turn it last had, so an ask Dispatch's settlement restores
-- reopens on that turn. The rows are Dispatch's and outlive the root's runs.
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
  check (open = (closed_by is null)));

-- design_gates.request_ask is the approval request a registration read open on the gate's
-- document (the ask Dispatch's approval read names while a request is awaiting), null when none
-- was ever open. A registration that finds none open keeps the one already recorded for the same
-- document; only a registration of another document rebinds it. Its root_asks row says whether
-- the request is still with the person.
alter table design_gates add column request_ask text check (request_ask <> '');

-- dispatch_issues.claimed_by is who Dispatch shows holding the issue's claim, `<kind>:<id>` ('' for
-- nobody), and claim_seq the Dispatch sequence of the observation that showed it (0 for none yet).
-- They carry a fence of their own, apart from dispatch_seq's: an issue event, a claim event, the
-- boot listing or the periodic re-read writes them only when its sequence is newer than claim_seq,
-- so a claim event, which says nothing of the status or the blockers, never moves those, and an
-- issue event older than a claim event already applied never rolls the claim back.
alter table dispatch_issues add column claimed_by text not null default '';
alter table dispatch_issues add column claim_seq bigint not null default 0 check (claim_seq >= 0);
