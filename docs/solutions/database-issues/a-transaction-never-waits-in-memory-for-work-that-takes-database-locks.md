---
title: "A transaction never waits in memory for work that takes a database lock: Postgres cannot see the wait, so it never breaks the cycle"
category: database-issues
tags:
  - postgres
  - deadlock
  - advisory-lock
  - concurrency
  - go
date: 2026-09-24
status: active
module: envoy
problem_type: database_issue
component: database
symptoms:
  - "A Dispatch API request on a document hangs until the client disconnects after the document's live room fails"
  - "pg_stat_activity shows a session waiting on pg_advisory_lock(hashtext(<artifact id>)) and no 40P01 deadlock is ever reported"
  - "The goroutine dump shows the request in awaitRoomRecovery and the room's eviction in Compact or AppendUpdate"
root_cause: concurrency
resolution_type: code_fix
severity: high
---

# A transaction never waits in memory for work that takes a database lock

## Problem

When a Dispatch document's live room fails, `failRoomLocked` evicts it: the room closes with a
flush, and ygo's per-room persistence worker then compacts the document on exit. Both the
flush's appends and the compaction go through `withRoomLock`
(`packages/envoy/internal/dispatch/docs/persistence.go`), which takes the document's advisory
lock (`pg_advisory_lock(hashtext(<artifact id>))`) at session level. Every reader of the failed
room waits in `awaitRoomRecovery` for that eviction to finish. This includes every `Apply` and
inject, through ygo's `OnInject` hook.

A handler's transaction holds the same lock, as a transaction lock, from its first
`AppendUpdateTx` or a conditional edit's `lockDocumentRoom` until it ends. Settlement's own
transaction holds it too. If that transaction then touched the failed room, it waited for the
eviction, and the eviction waited for the transaction's lock. The wait in `awaitRoomRecovery` is
a channel receive, which Postgres cannot see. So Postgres never reported a deadlock, and the
request held its connection and its locks until the client went away.

## Why "only when it holds this document's lock" is not the rule

The eviction needs the document's advisory lock and nothing else, beyond the foreign-key share
locks its inserts take. So it can look as though a transaction that does not hold that advisory
lock may wait safely. It may not. The lock's holder can itself be waiting on a Postgres lock the
waiting transaction holds.

For example, settlement holds the advisory lock and appends ask events, so it waits for the
global event lock. A handler that has already appended an event holds that lock and waits for
the room. That is a cycle with one invisible edge. Postgres breaks cycles made only of edges it
can see, so the rule has to cover any in-memory wait by any transaction.

## Fix

`awaitRoomRecovery` is where every such wait happens. It fails with `ErrServiceUnavailable`
(`503 DOC_SERVICE_UNAVAILABLE`) instead of waiting when the caller's ledger runs inside a
transaction (`Ledger.inTransaction`): a handler's joined transaction, or settlement's own. The
transaction rolls back, the eviction takes the lock, and the caller retries once the room has
reloaded. Callers outside a transaction hold no lock the eviction needs, so they still wait:
browser connections, reads, and a committed write's publish.

A joined write adds one more check (`applyJoined`). A room can fail between the write's fork
and its append, and the eviction can finish in that window, because the write does not hold
the advisory lock yet. The reloaded room may then lack the write, and nothing holds it off
between the commit and the publish, because the write's writer slot is on the failed room. So
after its append the write checks the room it holds the slot on (`liveWrite.roomFailure`). From
that point its lock keeps any later eviction from finishing until the transaction ends, so a
reload after it holds the write.

## Rule

An in-memory wait inside a database transaction (a channel, a mutex, a condition) is safe only
if the thing waited on can never itself wait on a database lock. When it can, fail fast and let
the transaction roll back. Compaction is not the only lock-taking step, so a fix that only skips
compaction leaves the flush's appends in the cycle.

## Tests

`packages/envoy/internal/dispatch/docs/room_recovery_test.go` bounds each wait with a
ten-second timer and checks for `ErrServiceUnavailable`:

- a transaction holding the advisory lock, on an issue document and on a project document;
- a transaction holding none of the document's locks while another transaction holds the
  advisory lock;
- a failure between two joined operations, and one inside a joined write;
- a settlement that holds the lock.

A writer waiting for another transaction's writer slot finishes when the room fails. The
blocking tests hang on a head without the fix, which is what makes them red.
