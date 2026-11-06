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
-- The mark is therefore a row of its own, keyed by the issue, the tree it waited in and the
-- generation of that tree's run, in the style of the design gate's told-review marks (0055). The
-- generation is the root's, not the waiting issue's: re-admission raises the root's and leaves
-- every member's where it stands, so a sub-parent that waited for its unreleased children before
-- the park would otherwise be marked told for ever and the new run's architect would never hear
-- of that wait. The tree is in the key because a generation only means a run within one tree: a
-- sub-parent handed over as a root of its own starts a run of its own, numbered from its own
-- record (admit's readmit), which can be the number the old root's run had — so without the tree
-- its first wait would find the old tree's mark and tell nobody. A child filed later during one
-- wait finds the row already there and tells nobody twice. Nothing deletes a row: the issues
-- table's own cascade does, when a record goes.
create table children_first_notices (
    issue text not null references issues (key) on delete cascade,
    tree text not null,
    generation bigint not null check (generation >= 0),
    told_at timestamptz not null,
    primary key (issue, tree, generation)
);
