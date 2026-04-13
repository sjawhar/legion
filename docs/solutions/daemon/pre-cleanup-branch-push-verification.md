---
title: "Pre-cleanup branch push verification prevents data loss"
category: daemon
tags:
  - data-safety
  - cleanup
  - jj-workspaces
  - destructive-operations
  - dependency-injection
  - fail-safe
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "489"
symptoms:
  - "workspace cleaned but PR branch had unpushed commits"
  - "implementation code lost during workspace cleanup"
  - "cleanupWorkspace destroyed work that wasn't pushed"
---

# Pre-cleanup branch push verification prevents data loss

## Context

Issue #469 revealed that the daemon could clean a workspace whose associated PR had unpushed
implementation code. The +572 line implementation was lost — only docs changes survived on the
remote branch.

## Pattern: Guard clause before destructive operations

Add a **pre-condition safety gate** before any destructive operation. The gate is a pure function
returning `{ safe: boolean; reason?: string }`, and the **caller** decides the error behavior.

### Two error modes by context

| Cleanup path | Error behavior | Rationale |
|---|---|---|
| `cleanupWorkspace()` (explicit request) | **Throw** (fail-hard) | Caller explicitly requested cleanup and needs to know it was refused |
| Server directory scan (background auto-cleanup) | **Log + skip** (fail-safe) | Throwing would disrupt the daemon loop; workspace survives until next cycle |

This distinction matters: background cleanup should never crash the daemon, but explicit cleanup
should surface the refusal clearly.

### Structured return over boolean

`{ safe: true }` vs `{ safe: false, reason: string }` is better than a bare boolean. The `reason`
string flows through to error messages and log lines without the caller needing to reconstruct
why cleanup was refused.

### Reuse existing DI boundaries

The `verifyBranchPushed` function accepts the same `RepoManagerDeps` interface that
`cleanupWorkspace` already uses. No new interface, no new injection point — just an additional
function using the same seam. This made testing trivial (11 new tests, zero new test infrastructure).

## Off-board workspace limitation

The directory scan fallback path in `server.ts` doesn't have a `repo` object for off-board
workspaces. The solution derives the clone path from any existing worker entry (all workers in a
team share the same repo). If no workers exist, the check is skipped (workspace survives).

This is pragmatic but fragile for multi-repo teams. The longer-term fix would be persisting repo
metadata alongside the workspace directory.

## Key decision: "no bookmark = safe"

When `jj bookmark list` returns nothing, the code treats it as "no bookmark exists, safe to clean."
This is correct — no bookmark means no work to lose. The explicit `exitCode !== 0` check adds a
second safety layer (jj failure = don't clean).

## Takeaway

For any destructive operation in the daemon, consider:
1. Can the operation lose user work? If yes, add a pre-condition check.
2. Is the caller explicit or background? Match error behavior to context.
3. Return structured results so callers can surface meaningful messages.
4. Reuse existing DI seams — don't create new injection points for safety checks.
