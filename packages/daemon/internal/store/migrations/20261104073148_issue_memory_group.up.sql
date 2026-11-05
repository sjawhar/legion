-- 20261104073148_issue_memory_group.up.sql — the tree's memory group, on the root's issue record.
--
-- A project's `memory` block says what each tree it opens gets (config.Memory): `off` gives no tree
-- a memory group, `on` gives every new tree one, and `random` draws for each new tree. The draw is
-- made once, when the tree's lifecycle row is first created, and is kept here: memory_group is the
-- group drawn (`memory` or `none`), memory_draw_mode the mode in force at that draw, and
-- memory_drawn_at when it was drawn. Every role of the tree runs under its root's group, so a child
-- stores none of its own, and a child handed over as a tree of its own takes a copy of the three.
--
-- memory_drawn_at is the draw itself: it is null exactly while no draw has been recorded. That is
-- what makes the three write-once, since record.Store.DrawMemory writes only where it is null and
-- record.Store.CarryMemory copies only onto a row that has no draw; and record.Store.PutIssue names
-- none of the three, so a copy of the issue read before the draw never writes it back, which is
-- what makes a re-admission, a new generation and ClearTreeGeneration keep the draw.
--
-- No backfill, by design. A row recorded before this migration has memory_drawn_at null, which
-- means no group and no memory: a tree open at this release keeps running without memory, as it has
-- until now, and nothing draws for it later. A change of a project's mode reaches only the trees
-- first opened after it.
alter table issues add column memory_group text not null default '';
alter table issues add column memory_draw_mode text not null default '';
alter table issues add column memory_drawn_at timestamptz;
