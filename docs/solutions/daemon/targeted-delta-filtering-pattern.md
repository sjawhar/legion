---
title: "Targeted Delta Filtering for Daemon State Notifications"
category: daemon
tags:
  - state-delta
  - tracking
  - notifications
  - backward-compatibility
  - testing
  - bun-server
  - typescript
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "469"
symptoms:
  - "controller receives changed/removed entries for 190+ unrelated board issues every cycle"
  - "delta notifications are noisy — controller can't distinguish relevant from irrelevant changes"
  - "adding a filter param to computeStateDelta breaks existing callers"
  - "existing 'publishes delta' tests fail after changing delta filtering behavior"
  - "auto-untrack doesn't fire for issues that have no workers"
---

# Targeted Delta Filtering for Daemon State Notifications

## Problem

The daemon's `computeStateDelta` function diffs all board issues every cycle. When the board has 190+ issues, the controller receives `changed`/`removed` entries for issues it has no workers on — pure noise. The fix is to filter `changed`/`removed` to a tracked set, but the design has several non-obvious constraints.

## Design: Asymmetric Filtering

**New issues always flow regardless of the tracked set.** The controller needs to see new work arriving on the board so it can triage and decide whether to dispatch. Only `changed` and `removed` entries are filtered.

```typescript
export function computeStateDelta(
  previous: Record<string, IssueStateDict>,
  current: Record<string, IssueStateDict>,
  trackedIssueIds?: Set<string>  // undefined = full delta (backward compat)
): StateDelta | null {
  // new: always include all new issues
  const newIssues = Object.keys(current)
    .filter((id) => !(id in previous))
    .map((id) => ({ issueId: id, state: current[id] }));

  // removed/changed: filter to tracked set when provided
  const removedCandidates = Object.keys(previous).filter((id) => !(id in current));
  const removed = (
    trackedIssueIds !== undefined
      ? removedCandidates.filter((id) => trackedIssueIds.has(id))
      : removedCandidates
  ).sort();

  // ... same pattern for changed
}
```

**Key invariant:** `undefined` preserves original full-delta behavior — no existing callers break.

## Auto-Track / Auto-Untrack Lifecycle

Track issues automatically at resource lifecycle boundaries:

```typescript
// Auto-track: immediately after POST /workers creates a worker
trackedIssueIds.add(normalizedIssueId);

// Auto-untrack: in cleanupDoneIssueWorkers, iterate doneIssueIds directly
// NOT cleanedIssueIds — that only covers issues that had workers to clean up
for (const issueId of doneIssueIds) {
  trackedIssueIds.delete(issueId);
}
```

**Gotcha:** If you place `trackedIssueIds.delete` inside the `cleanedIssueIds` loop, issues that were manually tracked (no workers) will never be untracked when they go Done. Always iterate `doneIssueIds` for untracking.

## Existing Tests Encode Old Behavior

When you change delta filtering semantics, existing "publishes delta" tests will break. They establish a baseline, change an issue's state, and assert a publish happened — but with the new filtering, untracked issues produce no `changed` entries.

**Fix pattern:** Add a `POST /state/track` call before the second collect in any test that expects `changed` entries to be published:

```typescript
// Before: test assumed changed entries always published
// After: track the issue first
await requestJson("/state/track", {
  method: "POST",
  body: JSON.stringify({ issueId: "acme-widgets-42" }),
});
// Now the second collect will produce a changed entry for this issue
```

Audit all tests in the "state delta notifications" describe block when changing delta behavior.

## Bun Server: `request.json()` Returns `unknown`

Bun's `Request.json()` returns `Promise<unknown>`. Accessing properties directly is a TypeScript error. Pattern for POST body validation in server handlers:

```typescript
const payload = await request.json().catch(() => null);
const payloadObj =
  payload && typeof payload === "object" ? (payload as Record<string, unknown>) : null;
if (!payloadObj || typeof payloadObj["issueId"] !== "string") {
  return badRequest("issueId is required");
}
const issueId = payloadObj["issueId"].toLowerCase();
```

Do not use `payload.issueId` directly — tsc will error even if biome passes.

## Accumulator Pattern for Pull-Based Consumption of Push Events

New issues arrive via delta events (push) but `GET /state/materialized` is pull-based. Bridge with a drain-on-read accumulator:

```typescript
const newIssuesSinceLastPoll: Array<{ issueId: string; state: IssueStateDict }> = [];

// In runPostCollectionProcessing, after computing delta:
for (const entry of delta.changes.new) {
  newIssuesSinceLastPoll.push(entry);
}

// In GET /state/materialized handler:
const newIssues = newIssuesSinceLastPoll.splice(0);  // drain atomically
return jsonResponse({ issues, titles, newIssues });
```

`splice(0)` drains the array in one operation — no race between reading and clearing.

## `.legion/` Rebase Conflicts

When rebasing a feature branch onto main, `.legion/` handoff files (review.json, implement.json, etc.) conflict because other issues' review/test commits land on main with their own `.legion/` files. Always resolve by keeping the branch version:

```bash
git checkout --theirs .legion/review.json
git add .legion/review.json
git rebase --continue
```

The branch's `.legion/` files are the correct per-issue handoff data. Main's versions belong to other issues.
