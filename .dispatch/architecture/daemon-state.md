---
title: Coordinator state and issue record
parent: daemon
depends_on: [daemon-supervision, daemon-identity]
paths: [packages/daemon/internal/store, packages/daemon/internal/registry]
---
Stages 1 and 2 on `main` provide Postgres migrations and boot history (`internal/store`), the local daemon registry, durable role claims, and pending task delivery (`internal/registry`). Stage 3 (LEGION-219) plans the per-issue record, admission slots, phase and gate facts, transactional outbox, and `internal/projection`; the coordinator never mirrors Dispatch or GitHub.

The issue record keeps each tree's memory group on the tree's root: migration `20261104073148_issue_memory_group` adds `memory_group` (`memory` or `none`), `memory_draw_mode` (the project's `projects.<KEY>.memory.mode` in force at the draw) and `memory_drawn_at` to `issues`, and backfills no row. `memory_drawn_at` is the draw itself, null exactly while none is recorded, which is what makes the three write-once: `record.Store.DrawMemory` writes only where it is null, `CarryMemory` copies a handed-over child's old tree's three onto a row that has none, and `PutIssue` names none of them, so a copy of the record read before the draw never writes it back and a re-admission, a new generation and `ClearTreeGeneration` keep what was drawn. A row written before the migration therefore carries no draw, which is a tree with no group that runs without memory and is drawn for by nothing later.
