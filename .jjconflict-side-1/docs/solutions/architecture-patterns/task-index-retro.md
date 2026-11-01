---
title: Task Index Performance — Implementer Retro
category: architecture-patterns
tags:
  - task-index
  - file-based-index
  - caching
  - bootstrap-bug
  - parallel-subagents
  - performance
date: 2026-02-15
status: active
module: daemon
related_issues:
  - LEG-51
---

# Task Index Performance — Implementer Retro

**PR:** https://github.com/sjawhar/legion/pull/51

## What Was Hard

### The empty-index bootstrap bug (P1)

The plan explicitly noted a "known gap — crash recovery" where a crashed process could leave a task invisible. The plan dismissed it as "rare and self-correcting." The reviewer caught that this same gap applies to the FIRST upsert — when no index file exists, `upsertIndexEntry` fell back to `{ version: 1, entries: [] }` and wrote an index containing only the single touched task. All other active tasks became invisible.

**Pattern:** When an optimization cache (index, bloom filter, summary) is missing, rebuild it from the source of truth — don't start empty. The fallback `?? emptyDefault` pattern is dangerous for caches that gate reads. `readTaskIndex() ?? rebuildIndexFromDisk()` is the correct pattern.

### Analysis agents made things worse

Five parallel analysis agents (type-checker, bug-finder, code-simplifier, code-reviewer, test-analyzer) ran on the completed implementation. The bug-finder agent added a `unlinkSync(indexPath)` on write failure — ostensibly to "force fallback to full scan." But since `writeJsonAtomic` already uses temp+rename (old file survives), the unlinkSync actively deleted a valid index. This compounded the P1 bug.

The code-simplifier agent also introduced an unnecessary Map-based dedup in `upsertIndexEntry`, replacing the plan's simpler `filter` + `push`. The reviewer correctly called this out as over-engineering.

**Pattern:** Analysis agents are good at finding surface-level issues (formatting, types, test coverage) but can introduce architectural regressions. Their changes should be reviewed with the same rigor as the original implementation. "More agents ≠ better code."

### Parallel subagent file conflicts

Tasks 4 and 5 were executed in parallel by separate subagents. This required careful analysis of file overlap:
- Task 4: task-list.ts, task-claim.ts, task-list.test.ts
- Task 5: types.ts, task-create.ts, task-update.ts, task-create.test.ts, task-update.test.ts

Zero overlap — safe to parallelize. But getting this wrong would have caused silent merge conflicts in jj.

**Pattern:** Before parallelizing subagent work, build an explicit file-overlap matrix. If ANY file appears in both task's write set, they must be sequential.

## Key Design Decisions

### readActiveTasks vs readAllTasks — why both exist

`readAllTasks` does a full directory scan. It's used by `task-create.ts` and `task-update.ts` for cycle detection, which needs the complete task graph including completed/cancelled tasks. `readActiveTasks` uses the index and only loads pending/in_progress tasks — the hot path for list and claim.

Merging these into one function with a boolean flag (`useIndex`) was the right DRY move, but the two public names (`readActiveTasks`, `readAllTasks`) must remain because they communicate intent at call sites.

### buildTaskMapWithBlockers — why it exists

`readActiveTasks` skips completed tasks. But dependency resolution needs to know if a blocker is completed (dependency satisfied) or missing (dependency not satisfied). `buildTaskMapWithBlockers` reads individual completed blocker files on demand — typically 0-3 per task. This is the key correctness mechanism that makes the index optimization safe.

Without it, replacing `readAllTasks` with `readActiveTasks` in the claim tool would treat completed blockers as "missing" (blocking), which is wrong.

### Description guardrail in Zod vs imperative

The initial implementation used imperative `if (validated.description.length > MAX)` checks in both task-create and task-update. The reviewer correctly suggested `.max(MAX_DESCRIPTION_CHARS)` on the Zod input schemas — single source of truth, automatically applies to new handlers.

**Gotcha:** Only add `.max()` to INPUT schemas, not `TaskSchema`. Otherwise, existing tasks > 2000 chars would fail `TaskSchema.parse()` in read paths.

Also required broadening the catch block from `error.message.includes("Required")` to `error.name === "ZodError"` to handle `.max()` validation errors.

## What Went Well

- The plan-review skill caught 7 issues in the draft plan before implementation started (import-append bug, correctness bug in readAllTasks replacement, wrong test expectations, nonexistent file references)
- Parallel execution of Tasks 4+5 saved ~2 minutes of wall time
- The `writeJsonAtomic` temp+rename pattern from storage.ts was correctly reused for index writes — no need to reinvent atomic file operations
- Moving from 6 tasks to 4 phases (batching 1+2, parallelizing 4+5) was the right optimization

## What I'd Do Differently

- Treat the "known gap" in the plan as a blocker, not a deferral. The bootstrap problem was foreseeable.
- Run fewer analysis agents with more focused prompts. Five parallel agents generated noise and introduced regressions. A single Oracle consultation on "what happens when the index doesn't exist yet?" would have been more valuable.
- Start with the Zod `.max()` approach for validation from the beginning, rather than the imperative check that had to be refactored.
