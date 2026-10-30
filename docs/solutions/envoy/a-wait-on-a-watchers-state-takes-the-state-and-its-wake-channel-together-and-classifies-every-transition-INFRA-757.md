---
title: "A wait on a watcher's state takes the state and its wake channel together, and classifies every transition"
category: envoy
tags:
  - concurrency
  - kvwatch
  - goroutines
  - testing
date: 2026-10-30
status: active
module: packages/envoy/internal/kvwatch
related_issues:
  - "INFRA-757"
  - "sjawhar/legion#2517"
---

# A wait on a watcher's state takes the state and its wake channel together, and classifies every transition

Go has no condition variable you can select on with a context, so a `WaitX` over mutable state is
built from a broadcast channel that is closed and replaced under the same lock that changes the
state. Five rules make it correct; each is a way the obvious version is wrong.

- **Take the state and the channel in one critical section.** The waiter reads every field it
  tests *and* the current channel under one `RLock`, then selects on the channel it took. A change
  made after that reading closed a channel the waiter already holds, so it is a wake-up the waiter
  will see. Close and replace only under the write lock, in the same critical section that writes
  the state.
- **Re-read after every wake when the predicate is not one-way.** A predicate that can go false
  again — a new generation clears completeness — needs a loop, not a single `select`. Ask the
  question explicitly: can this go false after it is true? A one-way predicate (readiness) is the
  case where a bare `select` is right.
- **Order the clauses as the consumer orders them.** The wait's clause order is part of its
  contract with the read it gates. `WaitComplete` tests the terminal error before completeness
  because `RolesHeldBy` does, so a watcher `Check` found on a recreated bucket never answers the
  wait nil while that view still refuses every read.
- **Every terminal state is an early return, so `ctx` is a bound and not the mechanism.**
  Enumerate the states in which nothing will ever satisfy the predicate — a first start that
  failed, an error recorded on the running watcher, the watcher's own end, a stop — and return
  each with its own error. A caller whose gate is bounded at 30 s otherwise pays that whole bound
  for a fault that was already final.
- **Classify every transition of the state the waiter reads, and write the reason in the code.**
  Each is either a wake or provably not one, and the proof belongs beside it. A transition that
  leaves a blocked waiter's own reading unchanged — installing a replacement watcher, which is
  still not complete — wakes nobody, and a `wakeComplete()` there is dead code. The rule is not
  "delete wakes that look useless": the same review round that deleted one dead wake added a
  missing one.

## Proving a wake

A wake's only observable effect is that a goroutine stopped waiting, so the naive test passes with
the wake deleted. Four devices make it bind:

- **Park the waiter before you turn the cause on.** Start the wait in a goroutine and wait until a
  stack trace shows it blocked in that function's `select` (`internal/goroutinetest`), then fire
  the cause. Otherwise the cause can land first and the call returns from its first state read,
  proving nothing.
- **Two bounds, the inner well inside the outer.** Bound the answer at a fraction of the ctx, and
  fail at the inner bound naming the cause. A test that waits out the ctx instead returns
  `context.DeadlineExceeded`, which reads like a legitimate answer.
- **Make the cause the only thing that can end the wait.** Leave out any self-repair the
  production object has (a rescan), and fake the narrowest observation the code reads rather than
  provoking the fault for real: deleting a bucket also ends the watcher and records a second
  terminal error, so the wake under test would no longer be the only one that could answer.
- **Test the case with no wake too**, and assert the absence: after the ctx expires, the watcher
  has recorded no error, so a terminal error that later leaks into that path fails the test.
- **Prove each test binds by removing its own wake.** Remove one `wakeComplete()` at a time in a
  scratch copy and run the package through `go test -overlay`; exactly that cause's test must
  fail, and no other.

## Evidence (INFRA-757, sjawhar/legion#2517)

`kvwatch.Watcher.WaitComplete` gates the listener's start on the role claim view being complete
rather than ready. Before `internal/kvwatch/waitcomplete_test.go` existed, the deep review deleted
all four early-return `wakeComplete()` calls in a scratch copy and `go test ./internal/kvwatch/...
./internal/store/` still passed: a later write to `err`, `watcher` or `stopped` that forgot its
wake would have cost the listener its whole 30 s startup bound with nothing red. With the five
tests, removing one wake at a time failed exactly its own test, each at the 2 s inner bound inside
a 5 s ctx, naming the cause.

Two of the five rules came from review rather than from the design: the clause order was a
thermonuclear finding, and the dead wake in `watch`'s switch was a review finding whose evidence
was that both the tester and the deep review removed it with every test still green.
