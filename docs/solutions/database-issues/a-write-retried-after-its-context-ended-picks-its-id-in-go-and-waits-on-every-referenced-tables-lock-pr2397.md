---
title: "A write retried after its context ended picks its id in Go, and an insert waits on every table its foreign keys reference"
category: database-issues
tags:
  - postgres
  - pgx
  - locks
  - retries
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/api
---

# A write retried after its context ended picks its id in Go, and an insert waits on every table its foreign keys reference

- An insert into a table with foreign keys takes a lock on every referenced table when it starts,
  even when the key column is null. A migration's `ACCESS EXCLUSIVE` lock on any referenced table
  therefore blocks the insert.
- When pgx gives up on a statement because its context ended, it sends Postgres a cancel request
  afterwards. If the lock is released before the cancel arrives, the first statement still commits.
  A blind retry can then write the row twice.
- So a write you retry after a context deadline chooses its id in Go and inserts with
  `on conflict (id) do nothing`; both attempts send the same id.
- Retry only what a retry can fix: a context deadline or cancel, and SQLSTATE classes 40 and 55.
  Never retry class 23 (constraint and foreign-key violations).
- Bound how many retries run at once. Each one holds or waits on a pooled connection for its whole
  wait, and during a lock window every write would add one.

## Evidence

sjawhar/legion#2397 records each related-item check in `related_checks`, which references
`projects`, `issues`, `artifacts` and `asks`. With `asks` locked, a check timed out and its record,
written under a one-second context, was lost: exactly the check the records exist to count. The fix
(dae0c162, then 1289c778) is `recordRelatedChecks`, `relatedRecordRetryable` and
`relatedRecordRetryLimit = 4` in `packages/envoy/internal/dispatch/api/suggestions.go:48-53`,
`:797`, `:860-884`, pinned by `TestRelatedCheckRecordBlockedPastItsSecondIsWrittenOnceTheLockIsReleased`.
