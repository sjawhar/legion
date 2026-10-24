-- 0052_parked_decisions.up.sql — a person's decision on a parked tree waits for its next admission.
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
