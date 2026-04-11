---
title: "Merge retry pattern for race-condition conflicts"
category: skill-patterns
tags:
  - merge
  - retry
  - race-condition
  - github-api
  - conflict-resolution
date: 2026-04-11
status: active
module: legion-worker
related_issues:
  - "411"
---

# Merge Retry Pattern for Race-Condition Conflicts

## Problem

When a merge worker runs `gh pr merge --squash`, the merge can fail because `main` moved since the last rebase. This is a race condition — another PR was merged between the worker's rebase and merge attempt. The `gh pr merge` command doesn't distinguish "you lost a race" from "you don't have permission" in its error output.

## Solution: PR State Inspection + Bounded Retry

After a merge failure, inspect the PR state to classify the failure before deciding what to do:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,mergeable,mergeStateStatus -R $OWNER/$REPO
```

### Classification

| `state` | `mergeStateStatus` | Meaning | Action |
|---------|-------------------|---------|--------|
| `MERGED` | (any) | Another process merged it | Success — proceed to post-merge steps |
| `OPEN` | `BEHIND` or `DIRTY` | Race condition — main moved | Retry (bounded) |
| `OPEN` | anything else | Permission error, unknown error | Escalate with `user-input-needed` |
| `CLOSED` | (any) | PR closed without merge | Escalate — unexpected state |

### Retry Loop

Each retry iteration:
1. Rebase onto latest main (`jj git fetch && jj rebase -d main`)
2. Verify clean rebase — `jj status` must show no conflicts
3. Push
4. Wait for CI to pass (fix any CI failures)
5. Retry merge

**Bound:** Max 2 retries. After exhaustion, escalate with `user-input-needed` and `Worker failed:` notification.

### Why bounded

Unbounded retries risk infinite loops on high-traffic repos where `main` moves faster than CI can run. 2 retries is enough for normal race conditions (where 1-2 other PRs merged). If it fails 3 times, something systemic is happening (e.g., a merge train, branch protection rules) that needs human attention.

### Why retry substeps don't notify

Only terminal exits (retry-exhausted, other-error, success) send `envoy_publish` notifications. Intermediate retry iterations are internal to the workflow — the controller doesn't need to know about retries in progress, only final outcomes.

## When to apply this pattern

Any workflow step that:
1. Can fail due to a race condition (resource moved between check and act)
2. Has an API to inspect the current state after failure
3. Can be retried with a fresh state (rebase, re-fetch, re-check)

The `mergeStateStatus` field is GitHub-specific. For Linear or other backends, find the equivalent state inspection mechanism.
