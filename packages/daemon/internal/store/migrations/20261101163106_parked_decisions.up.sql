-- 20261101163106_parked_decisions.up.sql — what a person decided on a parked tree waits for its architect.
--
-- A person's decision on a tree nobody in it could hear, which the daemon relayed to the
-- controller (a parked-decision notice), kept until the start task or catch-up that names it has
-- reached an architect. A row is keyed by the issue the decision was made on — the ask's issue,
-- which is a tree's root or one of its members — and never by a root: the tree a decision is owed
-- to is the tree its own issue stands in now, read from the record whenever a start or a catch-up
-- names it. That is what makes an issue changing trees cost nothing: a child a linger hands to
-- Legion, a parked child a person's todo orphans, a grandchild handed over beside its parent and a
-- queued root admission adopts into a live tree each take the decisions made on them with them,
-- in any order and with nothing to move.
--
-- A row stands in one of four states:
--
--   KEPT       no claim and no hold: the next start or catch-up of its issue's tree names it.
--   CLAIMED    claimed_by is the one outbox row (a root-architect start or a catch-up notice)
--              whose payload names it; no other row names it while that claim counts.
--   HELD       held_by_claim and held_by_delivery are the claim and the delivery id of the start's
--              task, queued on the architect's claim and not yet confirmed: the claim's write of
--              that delivery takes the row from the start row that named it.
--   TOLD       deleted: the catch-up's publish, or the held delivery's confirmation, deleted it.
--
-- One validity rule governs both naming and telling. A claim counts only while the outbox row's
-- issue stands in the same tree as the row's own issue, and a hold only while the claim's tree
-- does; the telling deletes a row only while the row's issue still stands in the teller's tree,
-- and otherwise clears its claim and hold, leaving it KEPT for its own tree to name. So a claim or
-- a hold an issue left behind holds nothing back, and a person's decision is never lost: telling
-- it twice is the lesser fault.
create table parked_decisions (
  issue text not null references issues (key) on delete cascade,
  ref text not null,
  decision text not null,
  actor text not null,
  at timestamptz not null,
  claimed_by bigint references outbox (id) on delete set null,
  held_by_claim text,
  held_by_delivery text,
  primary key (issue, ref));

-- The tree a row is owed to is read through its issue, so every read of a tree's rows joins
-- issues on it.
create index parked_decisions_issue on parked_decisions (issue);

-- A root's spec approval that its re-admission found below the spec's latest version, kept from
-- the old generation's gate, which the re-admission clears, for the new architect's start task.
-- It is keyed by the issue whose spec it is — the root itself — and claimed, held and told exactly
-- as a decision row is.
create table readmitted_approvals (
  issue text primary key references issues (key) on delete cascade,
  approved_version integer not null check (approved_version > 0),
  latest_version integer not null,
  claimed_by bigint references outbox (id) on delete set null,
  held_by_claim text,
  held_by_delivery text,
  check (latest_version > approved_version));
