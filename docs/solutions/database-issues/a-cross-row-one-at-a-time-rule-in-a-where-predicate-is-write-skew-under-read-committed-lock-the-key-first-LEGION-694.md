---
title: "A cross-row one-at-a-time rule in a WHERE predicate is write skew under READ COMMITTED: take an advisory lock on the shared key before the conditional update, and test it with two different rows on two connections"
category: database-issues
tags:
  - postgres
  - write-skew
  - read-committed
  - advisory-lock
  - outbox
  - concurrency
  - regression-test-shape
date: 2026-10-16
status: active
module: packages/daemon
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A cross-row one-at-a-time rule in a WHERE predicate is write skew under READ COMMITTED: take an advisory lock on the shared key before the conditional update, and test it with two different rows on two connections

- A rule of the shape "at most one row of this key may be claimed at a time" is not enforced by
  putting it in each row's own conditional update (`update … where id = $1 and not exists (select 1
  from … sibling … where sibling.lease_until > now())`). Two transactions claiming two *different*
  rows of one key update different rows, so neither waits on the other's row lock; each statement's
  `not exists` runs in its own READ COMMITTED snapshot, where the other's uncommitted lease is
  invisible; Postgres's recheck of a concurrently updated row covers only the row being updated,
  never a subquery over other rows. Both claims commit. No lock wait, no deadlock, nothing for the
  detector to report: write skew.
- Serialize the key, not the row: in the same transaction, before the conditional update, take a
  transaction-scoped advisory lock keyed by the shared key, selected over the row so the kinds the
  rule does not cover take none —
  `select pg_advisory_xact_lock(hashtext('<namespace>:' || <key>)) from <table> where id = $1 and kind = any($2)`.
  The second claimant blocks until the first commits; its update then takes a fresh snapshot, sees
  the lease, and refuses. The lock lives for the claim's short transaction alone, never across the
  work the claim admits, and a distinct namespace keeps it off any other advisory lock the process
  takes (`intake.ApplyFact`'s `hashtext('legion.apply_fact')`); no transaction takes both, so no
  lock ordering arises.
- The regression test must claim two different rows of one key from two connections, the first
  uncommitted while the second claims, and then count the rows under a live lease: exactly one.
  A test that claims the *same* row twice proves the row's own update atomicity, which any
  conditional update already gives, and passes against the write skew. Hold the first transaction
  open, run the second claim in a goroutine, wait on `pg_stat_activity` (`wait_event_type = 'Lock'`)
  or on its return, then commit the first; accept a refusal by return or by the second's own error,
  so the test pins the invariant rather than one fix. Add the negative control that a row outside the
  rule (a board row) is leased at once while the lock is held, so the lock never widens.
- Moving a rule "into the SQL so the list, the claim and the visibility read agree" is right, and is
  still not serialization: SQL gives per-row atomicity; cross-row exclusion needs the lock.

## Evidence

LEGION-694 (sjawhar/legion#1999) gave the daemon's outbox a per-issue lifecycle lane that runs an
issue's lifecycle rows one at a time. The first implementation enforced the rule in the lane
goroutine alone; the real-binary proof found that a second daemon (a rolling restart, or a seeded
foreign lease) would list and claim a due lifecycle row while its sibling ran under the other
daemon's lease, so the rule moved into the shared SQL predicate (`outboxLaneFree` in
`packages/daemon/internal/record/store.go`), which `DueRows`, `ClaimRow` and `HeldRows` all embed.
Every acceptance test and the whole daemon suite passed on one process.

The tester's round-1 red test, `TestTwoDaemonsClaimingDifferentLifecycleRowsOfOneIssueAtOnceLeaseOnlyOne`
(`packages/daemon/internal/record/store_test.go`), claimed a `supervise` suspend and the same issue's
`workspace_remove` from two connections with the first uncommitted: both leased —
`the issue's lifecycle rows under a live lease = [supervise workspace_remove]`. The implementer's
own `ClaimRow` test (`TestClaimRowLeasesADueRowExactlyOnceUntilItsLeaseLapses`) had claimed one row
twice and passed. Round 2 added the advisory lock as `ClaimRow`'s first statement
(`store.go`, the `pg_advisory_xact_lock(hashtext('legion.outbox.lifecycle:' || issue))` select over
the row for lifecycle kinds); the red test passed unmodified three times, and
`TestABoardRowClaimedWhileItsIssuesLifecycleClaimIsUncommittedIsLeasedAtOnce` showed a board row of
the same issue leased with no `pg_stat_activity` lock wait while the lifecycle claim was open;
neutralising the lock's kind filter fails that test. The lock-ordering read the tester recorded
(`.legion/LEGION-694/test.json` round 2): a claim takes the issue's advisory lock, then the row lock,
and waits on nothing else before commit; `FinishOutbox`, `RetryOutbox` and `ExpediteOutbox` take row
locks only; no cycle.

`docs/solutions/database-issues/issue-then-row-lock-order-prevents-deadlock.md` is the other
Postgres concurrency failure of this repository, a true deadlock between two lock orders that
Postgres detects and kills one side for; this one is its quiet sibling, where both sides succeed.
