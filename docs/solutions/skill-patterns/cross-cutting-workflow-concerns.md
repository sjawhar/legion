---
title: "Adding Cross-Cutting Concerns to Worker Workflows"
category: skill-patterns
tags:
  - skills
  - workflows
  - architecture
  - knowledge-injection
  - cross-cutting
date: 2026-04-05
status: active
module: worker
related_issues:
  - "238"
  - "218"
---

# Adding Cross-Cutting Concerns to Worker Workflows

## Problem

When a behavior needs to be added to all (or most) worker workflow phases — like knowledge injection, telemetry, or pre-flight checks — there's a tension between DRY (don't repeat yourself) and locality (each workflow should be self-contained for the agent reading it).

## Pattern: Shared Reference + Inline Configuration Tables

Extract the **algorithm** into a shared reference file under `references/`, while keeping the **per-phase configuration** (what data to use, what fallback rules apply) inline in each workflow as a markdown table.

**Reference file** (`references/<name>.md`): Contains the canonical, phase-agnostic procedure. Answers "how to do it."

**Workflow step** (inline in each `workflows/*.md`): Contains a keyword source table with `| Source | Fallback |` columns and a one-line reference to the shared algorithm via `@references/<name>.md`. Answers "what to do it with."

This separation works because:
- The algorithm is stable and shared — changes propagate to all phases automatically
- The configuration differs per phase — each workflow knows what data it has available
- Agents reading a single workflow see the full contract (sources + fallback rules) without needing to read the reference file for the common case
- The reference file is available for agents that need the detailed procedure

## Gotcha: Step Numbering Collisions

Workflow files use decimal step numbers (1.5, 1.6, 1.7, etc.) and numbering conventions vary across files. When adding a cross-cutting step:

1. Check which sub-step numbers are already taken in each workflow
2. Use the next available number that places the step in the correct physical position (before step 2, after step 1.x)
3. Accept that the same logical step may have different numbers across workflows (e.g., "Inject Learnings" is 1.5 in architect.md but 1.7 in implement.md)
4. Physical ordering in the file determines execution order, not the step number

Pre-existing non-monotonic numbering exists (e.g., review.md had 1.6 physically before 1.5). This is a known inconsistency — don't try to fix it during a cross-cutting addition, as renumbering existing steps risks breaking references.

## Gotcha: Graceful Degradation at Two Levels

Cross-cutting steps that depend on optional data (handoffs, index files, external state) must degrade silently at **both** levels:

1. **Algorithm level** (in the reference file): "If the index doesn't exist, skip entirely"
2. **Call site level** (in each workflow): "If handoff data is unavailable, fall back to [simpler sources]"

The reference file handles infrastructure failures (missing files, bad JSON). The workflow step handles data availability (empty handoff fields, missing phase data). Both must produce the same outcome: proceed without blocking.

## Gotcha: Handoff Field Dependencies Are Scattered

When a cross-cutting step references handoff fields from other phases (`filesChanged[]`, `trickyParts[]`, `concerns[]`), the dependency is documented only in the consuming workflow's keyword source table. If a handoff schema changes, there's no central registry to check. Mitigate this by keeping keyword source tables scannable and using field names that match the handoff schema exactly.

## Validation

This pattern was used successfully for knowledge injection (#238), adding injection steps to all 5 worker phases (architect, plan, implement, test, review) with a single shared algorithm and per-phase keyword source tables. The implementation was pure markdown with no TypeScript changes, confirming that cross-cutting skill-layer concerns don't require daemon modifications.
