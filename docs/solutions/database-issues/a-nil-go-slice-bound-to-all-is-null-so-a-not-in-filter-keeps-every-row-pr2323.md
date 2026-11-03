---
title: "A nil Go slice bound to all() is NULL, so a not-in filter keeps every row"
category: database-issues
tags:
  - pgx
  - postgres
  - arrays
  - delete
date: 2026-10-28
status: active
module: packages/envoy
---

# A nil Go slice bound to all() is NULL, so a not-in filter keeps every row

- pgx sends a nil slice as SQL `NULL`. `x <> all(NULL)` is never true, so a
  `delete … where key <> all($1::text[])` with a nil slice deletes nothing and returns no error.
- Before binding a slice to `<> all(…)` (or `= any(…)` where an empty match is meant), replace a
  nil slice with an empty one, and test the call with `nil` explicitly, since callers that "have
  nothing" usually pass nil.

## Evidence

Pull request #2323, `packages/envoy/internal/dispatch/dashboard/dashboard.go:267-269` (the guard
before `Prune`'s `where build <> all($1::text[])` at `:281`); the boot passes `nil` when no process
is live, and `dashboard_test.go` calls `Prune(ctx, pool, nil)` and expects the old builds pruned.
