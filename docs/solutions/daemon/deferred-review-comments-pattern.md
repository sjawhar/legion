---
title: "Pattern: Tracking and Executing Deferred PR Review Comments"
date: 2026-02-15
category: daemon
tags: [code-review, cleanup, workflow-pattern]
related-issues: [LEG-129, LEG-125]
---

# Pattern: Tracking and Executing Deferred PR Review Comments

## Context

After merging PRs #41-43 (controller separation, workspace fix, env cleanup), reviewers flagged items that weren't addressed before merge. LEG-129 collected these into a single cleanup issue — 21 additions, 2 files.

## The Pattern

Not all review comments need blocking fixes. For non-critical items:

1. **Label priority in review** (P2 = should fix, P3 = nice to have)
2. **Create issue with PR reference** — include exact file paths, line numbers, and concrete fix descriptions
3. **Merge original PR** without blocking on the deferred items
4. **Address in focused follow-up** as a single cleanup issue

**When to defer:** Validation tightening, documentation, non-blocking improvements where the fix is independent of the main change.

**When NOT to defer:** Correctness bugs, security concerns, breaking API changes.

## What Worked

**Structured issue with file-level specificity.** Each deferred item had priority, exact file/line references, and a concrete fix description. This made implementation trivial — no exploration needed.

**Pre-plan triage eliminated unnecessary work.** The original issue listed 3 items. During planning, item #2 (add comment explaining raw fetch in `initializeSession`) was discovered to be moot — a prior PR had already replaced raw fetch with the SDK. Saved wasted effort on a comment that would have been immediately wrong.

**Right-sized execution.** Two tasks (implement + verify), single commit, single session. No planning doc needed for a 3-line change. The plan was a 1:1 mapping from the issue description to code changes.

## Implementation Notes

**Validation at trust boundaries.** The `isAbsolute(workspace)` check goes in the HTTP handler where external input enters — not deeper in `spawnServe` or `initializeSession` where the error would be confusing. `isAbsolute("")` returning `false` means the single check covers both relative paths and empty strings.

**Test calibration for tiny changes.** Single test for the new validation path. No need to test every invalid variant (empty string, null, undefined) — `typeof` checks already cover those, and one example proves the guard works.

**Decision documentation.** Inline comment explaining *why* SDK client caching was skipped — future readers know it was considered, not overlooked.

## Anti-patterns Avoided

- Over-testing: Didn't add tests for cases already covered by existing `typeof` checks
- Over-documenting: Didn't write a separate doc explaining path validation philosophy
- Scope creep: Didn't refactor all validation into a shared validator class
- Staleness: Issue created and executed same day, so file/line references were still accurate

## Reusable Takeaway

**For tiny PRs (< 30 lines, single concern):** skip planning phase, write test first, verify with full suite + type check, commit with issue reference.

**For deferred review comments:** capture exact context in issue (PR number, file paths, line numbers), include priority from review, execute promptly before references drift.
