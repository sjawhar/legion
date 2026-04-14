---
title: "Workflow instruction ordering and stale workspace data in agent-operated systems"
category: skill-patterns
tags:
  - skill-authoring
  - workflow-design
  - merge
  - workspace-management
  - cleanup
  - failure-modes
  - agent-behavior
date: 2026-04-14
status: active
related_issues:
  - "532"
symptoms:
  - ".legion/ files landing on main after merge"
  - "handoff data from wrong issue trusted by worker"
  - "post-merge cleanup has no effect"
  - "workspace reuse causes stale state"
---

# Workflow Instruction Ordering and Stale Workspace Data

## Context

Data-driven analysis of 180 worker sessions across 72 issues revealed two systemic patterns
in agent-operated skill workflows that caused repeated failures.

## Pattern 1: Side-effect ordering relative to irreversible operations

### Problem

The merge workflow had a `.legion/` cleanup step positioned AFTER `gh pr merge --squash`. At that
point, the squash merge had already committed the files to main and the branch was deleted. The
cleanup created a local commit on a dead branch — a complete no-op.

This went undetected because 0/12 merge workers questioned the ordering. Agents follow instructions
literally. When instructions say "clean up after merge," they execute cleanup after merge, even
when the cleanup can no longer have any effect.

### Root cause

The original instruction explicitly said "preserve .legion/ files" during merge. This was correct
when handoff data needed to flow between pipeline phases. But once the merge phase begins, all
phases are complete — preservation becomes harmful because the files get squashed into main.

### Fix

Move cleanup to BEFORE the irreversible operation. In this case, step 3.5 (after conflict
resolution, before push). The files are still available in branch history (jj/git) if rework
is needed, but they don't land on main.

### Principle

**When writing multi-step agent workflows, map each step's side effects against the
irreversibility boundary.** Cleanup, filtering, and sanitization steps must execute BEFORE
the operation that commits the result (push, merge, deploy). Post-irreversible-operation
cleanup is a no-op or creates orphaned state.

Checklist for workflow authors:
1. Identify the irreversible operation (merge, deploy, publish)
2. List all side-effect steps (cleanup, filtering, validation)
3. Verify each side-effect step is ordered BEFORE the irreversible operation
4. If a step must run after (e.g., notifications), confirm it doesn't attempt to undo
   something the irreversible operation already committed

## Pattern 2: Stale workspace data from workspace reuse

### Problem

Workers trusted `.legion/` handoff files blindly. Because workspaces are reused across issues,
these files often contained data from a PREVIOUS issue. 10 occurrences across the analysis window.

Workers reading stale handoff data would:
- Follow outdated plans
- Reference wrong file paths
- Make decisions based on previous issue context

### Root cause

No issue-ID validation on handoff data. The `.legion/` directory persists across workspace
assignments, and the handoff schema didn't encode which issue the data belonged to.

### Fix (immediate)

Added staleness warnings to implement.md and test.md workflows: "Verify the handoff content
references YOUR issue before trusting it."

### Principle

**Any data that persists in a workspace across issue assignments is a staleness vector.**
Per-workflow warnings are a band-aid — the systemic fix is either:
- Clear `.legion/` on workspace assignment (daemon-level)
- Embed issue ID in handoff schema and validate automatically
- Both

## Additional learnings

### Agents don't discover tools they aren't told about

`jj-agent-status` existed and was useful for workspace orientation, but 0% of workers used it
because it wasn't in the startup sequence. Agent tool discovery is opt-in. If a tool matters
for a workflow, it must be explicitly listed in the workflow instructions.

### Error swallowing is an agent-specific anti-pattern

Agents frequently use `2>/dev/null || true`, empty catch blocks, and silent fallbacks to avoid
producing "errors." This optimizes for "no errors in output" over "correct behavior," causing
cascading silent failures. Explicit fail-fast guidance in workflow instructions is necessary
to counteract this tendency.

### Instructions with 100% compliance have amplified blast radius

When an instruction is wrong (e.g., "preserve .legion/ files"), agents comply 100% of the time.
Unlike humans, who might question or skip an instruction that seems wrong, agents execute it
faithfully. This means instruction bugs affect every single execution — review workflow
instructions with the same rigor as production code.
