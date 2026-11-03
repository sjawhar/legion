---
title: "Content-addressed rows garbage-collected at boot take a shared lock to write and an exclusive lock to prune"
category: database-issues
tags:
  - advisory-lock
  - garbage-collection
  - deadlock
date: 2026-10-28
status: active
module: packages/envoy
---

# Content-addressed rows garbage-collected at boot take a shared lock to write and an exclusive lock to prune

- When writers insert shared content rows with `on conflict do nothing` and then link them, and a
  pruner deletes rows nothing links, each writer's transaction takes
  `pg_advisory_xact_lock_shared(hashtext(<name>))` and the pruner's takes
  `pg_advisory_xact_lock(hashtext(<name>))` on the same name. Writers run together; a prune never
  deletes a row between a writer's insert-or-skip and its link.
- Insert the content keys in one sorted order, so two writers sharing keys wait on each other in
  turn rather than in a cycle.
- Write the owning row and its links in the transaction that writes the content, so an owner row
  present means an owner complete, and a restart that finds it writes nothing.
- Test it with many rounds of a write and a prune started together, then read every row the
  writer linked.

## Evidence

Pull request #2323, `packages/envoy/internal/dispatch/dashboard/dashboard.go:39` (`lockName`),
`:119` (shared), `:276` (exclusive); `TestPublishAndPruneTogetherLeaveThePublishedBuildReadable`
(`dashboard_test.go:378`). With the prune's lock removed, that test failed with a foreign-key
error.
