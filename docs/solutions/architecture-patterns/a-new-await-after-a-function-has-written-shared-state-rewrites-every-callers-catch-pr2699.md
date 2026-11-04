---
title: "A new await after a function has written shared state rewrites every caller's catch"
category: architecture-patterns
tags:
  - error-handling
  - all-or-nothing
  - shared-state
  - callers
  - partial-application
date: 2026-11-03
status: active
module: packages/pi-envoy
related_issues:
  - "sjawhar/legion#2699"
---

# A new await after a function has written shared state rewrites every caller's catch

Adding a confirmation, a validation or a flush after a function has already written to a map, a
registry or a field gives that function a failure mode it never had: it now throws with part of
its work applied. Every caller written against the old function reads the throw as "nothing
happened".

- Decide which the function is, and say so at its head: all-or-nothing, or partially applied.
  All-or-nothing is usually right, because the callers already assume it.
- All-or-nothing means undoing what *this call* wrote before rethrowing, and only that: record
  each entry as you write it, and when you undo, check the shared slot still holds your own value.
  Two calls can both pass an "is it already there" check and both write, when the check and the
  write are separated by an await; the loser must not close the winner's work.
- When a call parks on the new await, a second call for the same key must wait for the first
  rather than reading the shared state as it stands and reporting it done. Keep the in-flight
  promise per key and await it at the head of the function.
- Then read the callers' catches rather than assuming they are fine. The expensive ones are the
  catches that *undo a mark* on failure: paired with a function that leaves its work applied, they
  leave live state nothing owns.

## Evidence

sjawhar/legion#2699 (INFRA-436) added the server's round trip to `subscribe()` in
`packages/pi-envoy/extensions/envoy.ts`. Review round 1 found the consequence: a failed open left
its subjects in `subscriptions` while `subscribeNotice`'s catch unmarked them as the role's, on the
premise that nothing opened. The next registration then carried such a subject as a plain interest,
which `endRole` never closes, so a controller another session had replaced kept taking the role
topic's wakes.

The fix is an `opened` list, a `closeOpened()` that closes a subject only while
`subscriptions.get(subject)` is still this call's own subscription, and the same pair for the relay
and the agent stream control subject. The ownership check has its own row — two opens of one fresh
subject both pass the in-flight check, since neither writes the in-flight entry until after it has
awaited `connect()` — and with an unconditional close that row fails, the newer caller's subject
closed and its pump retry suppressed. The in-flight map (`confirming`) is what makes a second open
wait rather than answer "already open" over a subject the first may yet close.
