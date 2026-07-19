---
title: "Native auto-merge pattern for base changes"
category: skill-patterns
tags:
  - merge
  - auto-merge
  - race-condition
  - github-api
  - branch-protection
date: 2026-04-11
status: active
module: legion-worker
related_issues:
  - "411"
---

# Native Auto-Merge Pattern for Base Changes

## Problem

When a merge worker directly runs `gh pr merge --squash`, it becomes responsible for waiting on
requirements and retrying when `main` moves. GitHub can own both decisions: native auto-merge waits for
required checks and review, then merges when they are satisfied.

## Solution: Enable Native Auto-Merge + Inspect State on Failure

After rebase and push, enable GitHub auto-merge:

```bash
gh pr merge "$LEGION_ISSUE_ID" --auto --squash --delete-branch
```

Do not wait for checks or dispatch retro from the worker. The state machine handles the downstream
transition and retro dispatch after the PR merges.

If auto-merge cannot be enabled, inspect the PR state to classify the failure before deciding what to do:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,mergeable,mergeStateStatus -R $OWNER/$REPO
```

### Classification

| `state` | `mergeStateStatus` | Meaning | Action |
|---------|-------------------|---------|--------|
| `MERGED` | (any) | Another process or auto-merge completed | No-op; the state machine owns post-merge work |
| `OPEN` | `BEHIND` or `DIRTY` | Base changed after preparation | Rebase, push, and re-enable auto-merge |
| `OPEN` | auto-merge blocked | Auto-merge is disabled or unavailable | Escalate with `user-input-needed`; never bypass protection |
| `OPEN` | anything else | Permission error or unknown error | Escalate with `user-input-needed` |
| `CLOSED` | (any) | PR closed without merge | Escalate — unexpected state |

### Base Change Handling

For `BEHIND` or `DIRTY`, rebase onto the latest `main`, resolve any conflicts, push, and run the
auto-merge command again. Do not replace it with a direct merge command or an `--admin` override.

### Why Native Auto-Merge

The worker does not poll CI or own a conflict-retry merge loop. GitHub preserves the requested squash
merge and merges as soon as branch protection allows it. If it cannot enable auto-merge, the worker
escalates the exact failure instead of bypassing branch protection.

### Deployment Verification

Deploy-time: verify `gh pr merge --auto --squash` enablement under the implementer-App installation token on a scratch PR.

## When to apply this pattern

Any GitHub PR workflow that can delegate a merge until required checks and reviews are satisfied.

The `mergeStateStatus` field is GitHub-specific. For Linear or other backends, use the backend's equivalent state inspection mechanism and preserve the controller/state machine ownership of downstream work.
