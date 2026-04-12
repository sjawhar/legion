---
title: "Cross-Family Review Validates Subagent Output Against Plans"
category: delegation
tags:
  - cross-family-review
  - subagent-fidelity
  - plan-compliance
  - oracle-review
  - quality-gates
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "447"
symptoms:
  - "subagent implements plan partially — omits guards or error handling"
  - "plan has correct code but subagent drops subtle requirements"
  - "tests pass but production edge cases are unhandled"
---

# Cross-Family Review Validates Subagent Output Against Plans

## Problem

When delegating implementation to subagents with detailed plans, the subagent may implement the "happy path" correctly but omit guards, error handling, or subtle requirements that were explicitly in the plan. This produces code that passes tests but fails in production edge cases.

## Observed Pattern

In issue #447, the plan explicitly included:
1. `failedRoles` tracking — skip workers on role serves that failed `listActiveSessions()`
2. PATCH response checking — only emit `daemon.worker_reaped` after `patchRes.ok`
3. Dynamic `serveType` resolution — don't hardcode `"shared"` for all workers

The implementing subagent (GPT-5.4 via `deep` category) produced working code that passed 37 tests but:
- Dropped `failedRoles` entirely (only had `restartedRoles`)
- Emitted the feedback event regardless of PATCH success
- Hardcoded `serveType: "shared"` for all workers

All three were caught by the cross-family Oracle review (Claude Opus).

## Why This Happens

Subagents optimize for "get tests to pass" rather than "implement the full spec." When the plan has both obvious guards (restart check) and subtle guards (query failure check), the subagent tends to implement only the obvious ones. The subtle guards don't have corresponding test failures to drive their implementation.

## Mitigation

### 1. Cross-Family Review Is Non-Optional for Medium+ Complexity

The implement workflow's step 5 (cross-family review) caught all three issues. This validates that the review step is not ceremonial — it provides genuine coverage that tests alone don't.

### 2. Tests Should Cover Subtle Guards

The plan's testing section included a test for `failedRoles` but the subagent didn't implement it. When delegating both implementation and tests to the same subagent, the subagent may omit tests for the same guards it omitted in production code. Consider delegating test writing separately, or including explicit "must test X" assertions in the delegation prompt.

### 3. Delegation Prompts Should Flag Critical Guards

In the delegation prompt's "MUST DO" section, explicitly call out guards that are easy to miss:

```
MUST DO:
- Track failedRoles separately from restartedRoles
- Check patchRes.ok before emitting feedback events
- Resolve serveType dynamically, not hardcode "shared"
```

The plan had the right code but the delegation prompt didn't flag these as critical. Adding explicit "MUST DO" for subtle requirements improves fidelity.

## Generalization

When a plan is detailed enough to include code snippets, the delegation should verify the subagent used ALL snippets, not just the main implementation path. The cross-family review (Oracle or equivalent) is the verification mechanism. Skipping it for "simple" changes risks the exact class of bugs this caught.
