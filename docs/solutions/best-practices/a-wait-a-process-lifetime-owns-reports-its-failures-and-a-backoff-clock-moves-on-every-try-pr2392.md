---
title: "A wait a process lifetime owns reports its failures, and a backoff clock moves on every try"
category: best-practices
tags:
  - retry
  - backoff
  - observability
  - go
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/events
related_issues:
  - "sjawhar/legion#2392"
---

# A wait a process lifetime owns reports its failures, and a backoff clock moves on every try

- Retrying until success is right for a wait bound by a request's context. For a wait bound by the process's lifetime (a hub seed, a relay start), it is a silent hang. Give such a wait a failure bound that returns the error, and a failure log rate-limited to one line per interval.
- When a new mechanism replaces one that logged or returned errors, list what the old one reported. The new one must report at least as much.
- Set a rate-limited action's "last tried" time on every try, failed ones included. If it moves only on success, a refused action is due again at every trigger, and each try holds up its caller for the full timeout.

## Evidence

In sjawhar/legion#2392, `Horizon.Await` (`packages/envoy/internal/dispatch/events/horizon.go`) first surfaced an observation's error only when the caller's context ended. The hub seed and the relay's first start wait on the process lifetime, so a horizon pool that could not dial would have left them waiting with nothing logged. Main's `readCursors`, which it replaced, had logged and returned that error. Commit `05ac50b18a3f` logs a failed observation at most once every 30 s, and `Await` returns the error after `horizonAwaitFailFor` (10 s) of failures in a row.

`writePersisted` (`horizon.go:600`) first moved `persistedAt` only on a successful write. With the database refusing every write, ten advances of the horizon inside one second sent 19 writes, each holding its observer for up to 2 s. Commit `185bb85d5936` moves `persistedAt` on every try. `TestAFailedPersistIsTriedAgainOnlyOnceEveryPersistInterval` proves it.
