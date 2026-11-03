-- 20261104004500_children_first_notices.up.sql — the mark that a tree's architect has been told
-- its parent waits for its own children (INFRA-980, "Children first").
--
-- A parent's first phase does not start while Dispatch shows an open child under it, and that
-- wait is never stored: each admission pass judges it again from the mirror, so a mirror row
-- that has gone stale holds the parent for one pass and no longer. The notice is the opposite
-- case. It is told once and must stay told: every fact, tick, hold release and boot runs the
-- pass again, and a notice re-told on each of them would bury the architect's topic under one
-- sentence it already read.
--
-- The mark is therefore a row of its own, keyed by the issue and the generation of its run, in
-- the style of the design gate's told-review marks (0055). The generation is what makes a
-- re-admitted tree a new telling: re-admission opens a new run of the root, whose architect
-- writes its spec again and must hear what holds it again, while a child filed later during one
-- wait finds the row already there and tells nobody twice. Nothing deletes a row: the issues
-- table's own cascade does, when a record goes.
create table children_first_notices (
    issue text not null references issues (key) on delete cascade,
    generation bigint not null check (generation >= 0),
    told_at timestamptz not null,
    primary key (issue, generation)
);
