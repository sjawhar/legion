---
title: "State Machine Actions vs Display Categories: Check Blocking Before Actionable"
category: daemon
tags:
  - state-machine
  - display-logic
  - formatter
  - poll
  - testing
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "#398"
symptoms:
  - "stale worker-active issues appear as ACTIONABLE instead of BLOCKED"
  - "user-input-needed issues show up in wrong section"
  - "test passes but blocked issues not categorized correctly in production"
---

# State Machine Actions vs Display Categories: Check Blocking Before Actionable

## Context

When building the `legion poll` command, the formatter initially used `suggestedAction !== "skip"` as the gate for the ACTIONABLE section. This seemed correct but produced wrong output for stale `worker-active` and `user-input-needed` issues.

## The Problem

The state machine in `decision.ts` assigns *operationally correct* actions that don't map directly to *display categories*:

| Issue State | State Machine Action | Expected Display |
|---|---|---|
| `user-input-needed` label, has feedback | `relay_user_feedback` | **BLOCKED** |
| `worker-active` label, no live worker | `remove_worker_active_and_redispatch` | **BLOCKED** |
| Normal todo issue | `dispatch_planner` | **ACTIONABLE** |
| Completed issue | `skip` | **SUMMARY** |

The first two rows have non-`skip` actions but should display as BLOCKED. A naive `action !== "skip"` check puts them in ACTIONABLE.

## The Rule

**When formatting state machine output for display, check label-based blocking conditions BEFORE checking `suggestedAction`.** The correct precedence is:

1. `labels.includes("user-input-needed")` → BLOCKED
2. `labels.includes("worker-active") && !hasLiveWorker` → BLOCKED
3. `isBlocked === true` → BLOCKED
4. `suggestedAction !== "skip"` → ACTIONABLE
5. Everything else → SUMMARY

This ordering treats blocking conditions as semantic overrides — the issue may have an actionable machine action, but from the consumer's perspective it's stuck.

## Test Fixture Rule

**Test fixtures for state machine consumers must match real state machine output.** The initial test used:

```typescript
// WRONG: impossible state in production
makeIssue({
  suggestedAction: "skip",
  labels: ["worker-active"],
  hasLiveWorker: false,
})
```

The state machine never produces `suggestedAction: "skip"` for a stale worker-active issue — it produces `remove_worker_active_and_redispatch`. The test passed but didn't catch the precedence bug because the fixture modeled an impossible state.

The correct fixture:

```typescript
// RIGHT: matches real state machine output
makeIssue({
  suggestedAction: "remove_worker_active_and_redispatch",
  labels: ["worker-active"],
  hasLiveWorker: false,
})
```

**Before writing test fixtures for state machine consumers, check `decision.ts` to verify what action each scenario actually produces.**

## When This Applies

Any code that:
- Formats state machine output for human consumption (CLI, dashboard, notifications)
- Categorizes issues into groups (actionable, blocked, waiting, done)
- Builds compact summaries from `IssueStateDict` data

This does NOT apply to code that executes state machine actions (the controller), which should use `suggestedAction` directly.
