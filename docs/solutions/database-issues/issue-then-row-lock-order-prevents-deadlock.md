---
title: "Every handler must lock the issue row before any child row, or concurrent requests deadlock"
category: database-issues
tags:
  - postgres
  - deadlock
  - lock-ordering
  - concurrency
  - go
date: 2026-09-09
status: active
module: envoy
problem_type: database_issue
component: database
symptoms:
  - "Postgres error 40P01 deadlock detected under concurrent requests against the same issue"
  - "One code path (document edits) locks the issue row before a child row; another (ask/comment actions) locked the child row before the issue row"
  - "The deadlock only reproduces under real concurrency — a single-request test never sees it"
root_cause: concurrency
resolution_type: code_fix
severity: high
---

# Every Handler Must Lock the Issue Row Before Any Child Row, or Concurrent Requests Deadlock

## Problem

Dispatch's transactional document mutations lock the issue row first
(`requireOpenIssue`, `packages/envoy/internal/dispatch/api/server.go:332-336`, a `select …
from issues where key = $1 for update`) and then the artifact/comment row inside the same
transaction (e.g. `packages/envoy/internal/dispatch/api/artifacts.go:121-125`). Before the
fix, `answerAsk` and `commentAction` did the opposite: they locked the specific ask or
comment row first (`loadAskForUpdate`/`loadCommentForUpdate`, a `for update` on that row) and
only *then* called `requireOpenIssue` to lock the issue row. Two transactions taking the same
two locks in opposite orders is the textbook Postgres deadlock: a concurrent document edit
holding the issue lock and waiting on the ask row, alongside an answer request holding the ask
row and waiting on the issue lock, deadlock each other — Postgres detects the cycle and kills
one side with `40P01`.

## What Didn't Work

Nothing upstream caught this with ordinary tests: each handler's own unit tests exercise one
request at a time and never contend for both locks simultaneously, so the inverted order looked
correct in isolation. It surfaced only in a scoped thermonuclear review of the fix commit that
introduced the modern `requireOpenIssue` gate on the ask/comment paths (see
`docs/solutions/controller/thermonuclear-pair-follows-the-code.md`).

## Solution

Fixed by loading the ask/comment row *unlocked* first, then acquiring the issue
lock, then acquiring the row lock — matching the edit path's order everywhere
(`packages/envoy/internal/dispatch/api/asks.go:199-208`,
`packages/envoy/internal/dispatch/api/comments.go:280-289`):

```go
unlockedAsk, err := s.loadAsk(r.Context(), tx, r.PathValue("id"))   // no lock yet
// ...
if err := s.requireOpenIssue(r.Context(), tx, unlockedAsk.IssueKey); err != nil { // issue lock first
    s.writeHandlerError(w, err)
    return
}
ask, err := s.loadAskForUpdate(r.Context(), tx, r.PathValue("id"))  // row lock second
```

The deterministic regression test that proves the ordering
(`packages/envoy/internal/dispatch/api/interactions_test.go:251-290`,
`TestAnswerAskLocksIssueBeforeAskRow`) uses a two-transaction recipe rather than a race
that only sometimes reproduces:

```go
edit, _ := database.Pool.Begin(ctx)
edit.QueryRow(ctx, `select key from issues where key = $1 for update`, issue.Key).Scan(&lockedKey)
go func() { responses <- dispatchRequest(t, handler, http.MethodPost, ".../answer", ...) }()
waitForDatabaseLocks(t, database, 1) // poll pg_stat_activity for wait_event_type = 'Lock'
edit.Exec(ctx, `update asks set anchor = anchor where id = $1`, ask.ID) // proves edit tx still owns other rows
edit.Commit(ctx)
response := awaitResponse(t, responses) // must now complete 200, not deadlock
```

`waitForDatabaseLocks` (`server_test.go:112-129`) polls
`select count(*) from pg_stat_activity where wait_event_type = 'Lock'` until the concurrent
request is actually blocked, which is what makes the test deterministic: it never proceeds
until the handler is provably waiting on the issue lock the other transaction holds, so the
test would hang and fail (not silently pass) if a future change reintroduced the wrong order.

## Why This Works

Postgres deadlock avoidance is entirely the application's responsibility for row-level locks —
Postgres only detects and breaks deadlocks after they happen, at a real cost (a failed request,
retried or surfaced as an error). The only reliable prevention is a single, repo-wide lock
order that every transactional handler follows: parent (issue) before child (ask/comment/
artifact row). One handler getting the order backwards is enough to deadlock against every
other handler that follows the correct order, because a deadlock needs only one cycle.

## Prevention

- Any new handler that takes both an issue-row lock (`requireOpenIssue`) and a child-row lock
  in the same transaction must acquire them issue-then-row, matching every existing handler.
  Load the child row unlocked first if you need its data (e.g. its `issue_key`) before you can
  call `requireOpenIssue`.
- When adding a regression test for a lock-ordering fix, use the two-transaction-plus-
  `pg_stat_activity`-poll recipe above rather than a bare goroutine race — it fails
  deterministically (a timeout) if the ordering regresses, instead of only sometimes catching
  it.

## Related Issues

- `sjawhar/legion#826`; fixed in a follow-up commit found by the scoped thermonuclear pair on
  the fix commit described in the ledger.
- `docs/solutions/controller/thermonuclear-pair-follows-the-code.md` — the review-process
  learning from the same finding.
