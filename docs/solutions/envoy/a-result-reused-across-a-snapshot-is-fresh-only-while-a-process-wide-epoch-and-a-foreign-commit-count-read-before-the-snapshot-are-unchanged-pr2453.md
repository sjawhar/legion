---
title: "A result reused across a snapshot is fresh only while a process-wide epoch and a foreign-commit count, read before the snapshot, are unchanged"
category: envoy
tags:
  - reuse
  - cache
  - crdt
  - exactness
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2453"
---

# A result reused across a snapshot is fresh only while a process-wide epoch and a foreign-commit count, read before the snapshot, are unchanged

Extends docs/solutions/envoy/a-settlement-reuses-an-earlier-result-only-for-the-same-tree-with-an-unshrinking-record-that-nothing-changed-it-pr2416.md.

When one party computes a result from a snapshot of shared state (a write's fork of a room) and another party later uses it in place of its own work (the room's replica after the write's update):

- Give the holder of the shared state an **epoch drawn from one process-wide counter**. Draw a fresh one whenever the holder starts again (a reload or a recovery). A counter kept per object starts again at the same value, so the replica of a reloaded room would match a read of the old one.
- Keep a **count of the commits made by any writer other than the one reusing**, raised in commit order under the holder's lock.
- Read both **before** the snapshot is taken, never after: a commit landing between the snapshot and a later read is in the holder and not in the snapshot.
- Reuse only when both still match at the moment of use, and under the holder's lock with nothing pending (here `renderedReplica.hold`). Anything else falls back to the original computation.
- When the reuse path skips the work, it still moves the holder's cached state forward exactly as the work would have.
- Test each guard by breaking it on purpose. Each break must make a test fail on the actual output, not only on an assertion about the guard.

## Evidence

- `packages/envoy/internal/dispatch/docs/tree.go:377` (`var replicaEpochs atomic.Uint64`); `standsAt` at `tree.go:538`; the foreign count in `renderedReplica.consumed`; `recover` draws a fresh epoch.
- `livewrite.go:409`: "The counters before the encode, never after it". `warmFork` takes the mark inside the room's `srv.Apply` callback, before `EncodeStateAsUpdateV1`.
- `changedBy` sets `r.markdown` to the reused rendering, so the next update is measured against it.
- Negative controls in sjawhar/legion#2453's body: with the epoch kept per replica, the reloaded-room case fails (`room 1 commit 1: classified content_changed=true, the shadow says false`). With the foreign count ignored, the browser-edit and service-commit cases fail on the rendered bytes. The tester built a head whose reuse path did not move the rendering forward, and 9 update classes differed from the base.
