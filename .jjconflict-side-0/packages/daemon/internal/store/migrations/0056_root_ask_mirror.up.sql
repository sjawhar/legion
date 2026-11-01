-- 0056_root_ask_mirror.up.sql -- the daemon mirrors Dispatch's state of every ask of a root and
-- each issue's Dispatch claim. Nothing in the daemon reads any of it to decide anything yet: the
-- tables are kept current so a later release can decide from them. Both are new tables and no
-- existing table changes, so a daemon of an earlier release runs on this schema unchanged.
--
-- root_asks mirrors Dispatch's state of each ask of a root, written from the ask's own events (and
-- from a design gate registration's read of the request it found open). It is level data, not a
-- queue of events, and each part of it has a fence of its own, the issue's event sequence of the
-- newest event that gave that part, so an event older than the part changes none of it and the
-- same events in any order leave the same row:
--   open, fenced by seq: whether the ask is open, from the newest event or read that said so
--     (0 while none has: a row only a turn reached).
--   waiting_on, fenced by waiting_on_seq: the turn Dispatch's own rule gives the ask after the
--     newest event that names one: a reply's turn, the person's for a reply on a closed ask (which
--     records no turn, so Dispatch falls back to the person's) and for a hand-back, and the agent's
--     for a version that moved an approval request past the version handed to the person. A close
--     names no turn, so a closed ask keeps it, and an ask Dispatch's settlement restores reopens on
--     it; an open ask no event gave a turn is with the person, as an ask opens.
--   closed_by, fenced by closed_seq: the newest close of the ask (who closed it: a person, a
--     session of the tree, Dispatch's settlement, anyone else), kept when the ask reopens. A reply
--     on a closed ask says the ask is closed but not who closed it, so a closed row whose close has
--     not arrived yet has none.
-- kind (null until an event names it, and null for a kind this daemon does not know), artifact
-- (the document an approval request asks a person to approve, null for a question) and by_tree (a
-- session of the root's tree opened the ask) are facts of the ask, kept whatever order its events
-- arrive in. A design gate's approval request is an approval row whose artifact is the gate's
-- document. person_answer_seq is the newest answer of the ask by a person, and person_seq the
-- newest other act of a person that would answer a root waiting on that person: a resolution, or a
-- reply that does not leave the ask with the person (a comment on a closed ask included). A
-- person's answer would answer such a root unless the ask is an approval request of the gate's
-- document, whose answer is a review that answers through its own artifact event; the row keeps
-- both, and the reader compares artifact with the gate's document. The rows are Dispatch's and
-- outlive the root's runs.
create table root_asks (
  issue text not null references issues (key) on delete cascade,
  ask text not null check (ask <> ''),
  kind text check (kind in ('question', 'approval')),
  artifact text check (artifact <> ''),
  open boolean not null,
  seq bigint not null check (seq >= 0),
  waiting_on text check (waiting_on in ('human', 'agent')),
  waiting_on_seq bigint not null default 0 check (waiting_on_seq >= 0),
  closed_by text check (closed_by in ('person', 'tree', 'settlement', 'other')),
  closed_seq bigint not null default 0 check (closed_seq >= 0),
  by_tree boolean not null default false,
  person_seq bigint not null default 0 check (person_seq >= 0),
  person_answer_seq bigint not null default 0 check (person_answer_seq >= 0),
  primary key (issue, ask),
  check (seq > 0 or not open),
  check ((waiting_on is null) = (waiting_on_seq = 0)),
  check ((closed_by is null) = (closed_seq = 0)));

-- dispatch_claims is who Dispatch shows holding each issue's claim, `<kind>:<id>` ('' for nobody),
-- and claim_seq the Dispatch sequence of the observation that showed it. An issue event, a claim
-- event, the boot listing or the periodic re-read writes it only when its sequence is newer than
-- claim_seq, so an issue event older than a claim event already applied never rolls the claim
-- back. It is a table of its own, keyed by the issue with no reference to dispatch_issues, so a
-- claim event that arrives before the issue's first mirrored observation is kept (that older
-- observation, once redelivered, carries an older claim), and a claim event never makes a
-- dispatch_issues row, which admission would read as an issue with no blockers. The claim of a
-- key with no dispatch_issues row is read by nothing until that row arrives.
create table dispatch_claims (
  key text primary key check (key <> ''),
  claimed_by text not null,
  claim_seq bigint not null check (claim_seq > 0));
