-- 0052_parked_decisions.up.sql — what a person decided on a parked tree waits for its next admission.
--
-- A person's decision on a tree nobody in it could hear, which the daemon relayed to the controller
-- (a parked-decision notice), kept until the root's next admission names it in its architect's
-- start task. A root keeps one row per Dispatch reference.
create table parked_decisions (
  root text not null references issues (key) on delete cascade,
  ref text not null,
  decision text not null,
  actor text not null,
  at timestamptz not null,
  primary key (root, ref));

-- A root's spec approval that its re-admission found below the spec's latest version, kept from
-- the old generation's gate, which the re-admission clears, for the new architect's start task.
create table readmitted_approvals (
  root text primary key references issues (key) on delete cascade,
  approved_version integer not null check (approved_version > 0),
  latest_version integer not null,
  check (latest_version > approved_version));
