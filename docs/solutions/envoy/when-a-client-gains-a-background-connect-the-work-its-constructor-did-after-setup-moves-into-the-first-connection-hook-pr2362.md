---
title: "When a client gains a background connect, the work its constructor did after setup moves into the first-connection hook"
category: envoy
tags:
  - nats
  - background-connect
  - reconnect-hook
  - goroutine-lifecycle
  - forward-merge
date: 2026-10-28
status: active
module: packages/envoy/internal/bus
---

# When a client gains a background connect, the work its constructor did after setup moves into the first-connection hook

- When a constructor learns to return before it has connected, list everything it did after its
  setup step (here: start the stream keeper after the stream reconcile). Each item now runs in the
  first-connection hook, after that hook's own setup has succeeded, never in the constructor's
  background branch. Started early, it races the setup it was meant to follow.
- Read a forward merge of such a change as a design change for every caller on your branch, even
  when the conflict is only a few lines: the conflict shows where code moved, not what now runs
  first.
- A goroutine a hook may start after its owner has stopped: check for the stop and record its done
  handle in one step, under the lock `Close` reads the handle with. `Close` stops first and reads
  second; a nil handle means nothing was started and there is nothing to wait for.
- Prove the ordering with the server down at construction and started after, and a very short
  period for the background loop, so a pass before the setup would show every run.

## Evidence

Merging `main` brought sjawhar/legion#2390: Dispatch's NATS clients connect in the background and
the owning client reconciles `ENVOY_NOTIFICATIONS` in a reconnect hook (`ensureStreamOnce`,
`packages/envoy/internal/bus/nats.go`). The merge started the keeper in the constructor's background
branch, before that reconcile. The keeper only adds subjects, so a pass that read the stream before
the reconcile and wrote after it put back a subject the reconcile had replaced, and nothing removed
it again. Commit 8ec4e3643684 starts the keeper from `ensureStreamOnce` after the reconcile
succeeds, with `startKeeper` and `awaitKeeper` sharing `c.mu` (`keeper.go`).
`TestTheKeeperOfABackgroundClientRunsNoPassBeforeItsFirstReconcile` (1 ms keeper period, server
started after construction) passed 90 of 90 runs; with the old placement 23 of 30 failed.
`TestStoppingABackgroundClientBeforeItsFirstConnectionStartsNoKeeper` covers a stop before the first
connection.
