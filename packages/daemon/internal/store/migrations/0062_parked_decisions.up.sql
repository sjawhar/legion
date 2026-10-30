-- 0062_parked_decisions.up.sql — what a person decided on a parked tree waits for its root's architect.
--
-- A person's decision on a tree nobody in it could hear, which the daemon relayed to the controller
-- (a parked-decision notice), kept until the architect start task or catch-up that names it has
-- reached an architect. A root keeps one row per Dispatch reference. claimed_by is the one outbox
-- row (a root-architect start or a catch-up notice) whose payload names the row: no other start or
-- catch-up names a claimed row until that row settles it. A start whose task the root architect's
-- claim queued, and a published catch-up, delete what they named; a start that reached no claim,
-- and a catch-up a newer one supersedes, hand their rows over — unclaimed again, and named at once
-- by a fresh catch-up while the root has an architect to tell. A row's deletion releases what it
-- still claims (on delete set null), for the next start or the next ready's catch-up to name.
create table parked_decisions (
  root text not null references issues (key) on delete cascade,
  ref text not null,
  decision text not null,
  actor text not null,
  at timestamptz not null,
  claimed_by bigint references outbox (id) on delete set null,
  primary key (root, ref));

-- A root's spec approval that its re-admission found below the spec's latest version, kept from
-- the old generation's gate, which the re-admission clears, for the new architect's start task,
-- and claimed by that start as a decision row is.
create table readmitted_approvals (
  root text primary key references issues (key) on delete cascade,
  approved_version integer not null check (approved_version > 0),
  latest_version integer not null,
  claimed_by bigint references outbox (id) on delete set null,
  check (latest_version > approved_version));
