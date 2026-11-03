---
title: "The facts one decision compares come from one read"
category: database-issues
tags:
  - torn-read
  - postgres
  - atomic-snapshot
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2396"
---

# The facts one decision compares come from one read

- A check that compares several facts reads them all in one SQL statement. One example is the
  newest event id against the row at a reader's cursor. Two adjacent statements can each be
  right and still describe two states of the table.
- In Postgres a statement holds its ACCESS SHARE lock until it ends. A `truncate … restart
  identity` (ACCESS EXCLUSIVE) can therefore commit between two statements, but never inside one.
  Use a `left join lateral` or scalar subqueries to get the facts in one statement.
- In memory, facts a reader compares are published together. Use one `atomic.Pointer[state]`
  that a writer replaces whole, in a compare-and-swap loop. Never use separate fields that change
  one at a time. A reader of two fields can get a credential from one moment and a failure reason
  from another.
- Write the regression test with the change between the two reads: a reset or a write injected
  right after the first read. Without the fix it must fail every time, not only sometimes under load.

## Evidence

- `packages/envoy/internal/dispatch/api/delivery_live_events.go:71-81`: `eventCursorCheck` is one
  statement now. Main's two-statement check failed the new test 3 of 3 with a truncate between
  the reads, and the live event stream took a reset log for a second reset.
- `packages/daemon/internal/agentsecrets/client.go:76` (`state atomic.Pointer[loginState]`), `:91`
  (`snapshot`), `:100` (`update`) and `:275` (`LoginStatus`). A 20 ms split between the two field
  reads failed 5 of 5 with "no token login yet".
