---
title: "Derive a refusal's condition from what the caller depends on, not from the mechanism that broke it"
category: architecture-patterns
tags:
  - invariants
  - error-handling
  - concurrency
  - errgroup
  - review-round
date: 2026-10-25
status: active
module: envoy
applies_when:
  - A review or a test has shown one way a result can be wrong, and you are about to check for it
  - You are writing a bounded fan-out whose output is read as a membership set
  - You are choosing between a condition on the symptom and a condition on the requirement
related_issues:
  - "INFRA-249"
  - "sjawhar/legion#2191"
---

# Derive a refusal's condition from what the caller depends on, not from the mechanism that broke it

## Rules

- **State what the caller requires, then refuse unless it holds.** A fix phrased "check for the
  thing that went wrong" is a guard: it catches this mechanism and lets the next one through. A
  fix phrased "the caller requires X, so refuse unless X" is the invariant, and only the second
  survives a mechanism nobody has met yet. The two are easy to tell apart by reading the
  condition aloud.
- **An `errgroup.Wait()` of `nil` does not mean every item ran.** `Wait` reports only the errors
  its workers returned, never the parent context's, so a launch loop that stops on
  `groupCtx.Err()` leaves items unlaunched with nothing to show for it. Count what was decided
  and compare it with what was requested; do not test the deadline.
- **Pin a budget's refusal with an injectable budget, not the wall clock.** A test that races a
  five-second timeout is a test nobody trusts and nobody reruns.
- **A result that may be incomplete cannot speak for what is absent from it.** Before reading a
  missing key, a missing row or a missing item as a "no", establish that the thing that produced
  the collection was complete.

## Evidence

`resolveBroadcastSessions` (`packages/envoy/internal/dispatch/api/broadcasts.go`) resolves each
selected recipient by its own id, eight reads in flight under one five-second budget. Its launch
loop broke out on `groupCtx.Err()`, so a budget that ended between launches returned a **partial
map with no error**: `resolveBroadcastTargets` then read every unlaunched id as `no live session`,
and a send with one recipient left committed those exclusions under its idempotency key, where
`replayedExclusions` answers `excluded by the original send` to every retry of that key. A session
dropped because time ran out, recorded as left out on purpose, permanently for that key.

Two reviewers and the tester each proposed the same fix: test `ctx.Err()` after `Wait`. That
catches the deadline. What the caller depends on is not "the deadline did not pass" but "every
selected session was judged against its own record", so the refusal counts the ids it decided —
a record found, or the store saying it holds none — and refuses when that count is not
`len(requested)`, wrapping `ErrUnavailable` so the send answers `503 ENVOY_UNAVAILABLE` and writes
nothing. The count also refuses an id a future change drops for a reason that has nothing to do
with time; the deadline check would not. The tester later found one such reason already present:
a client disconnect, which has no deadline in it.

`TestBroadcastWhoseBudgetEndsBeforeEverySessionIsReadWritesNothingAndLeavesItsKeyFree` pins it
through an unexported `server.broadcastResolveBudget` (zero means the production five seconds;
only a test sets it), and asserts the refusal, no broadcast row, no message row, no listener send,
and that the same idempotency key is accepted afterwards under the default budget. Deleting the
refusal makes it answer `400 BROADCAST_EMPTY … no live session`, the defect itself.

The plan had already written the budget clause ("a budget that ends refuses the send") and shipped
round one with no test for it. **A timeout clause in a plan comes with its deterministic test in
the same round**, or it is a sentence rather than a behaviour.

## The same seam from two other directions

Three notes of one night, each found by someone refusing the obvious fix. They are the same
question asked about different objects, so read whichever one your case resembles and then the
others:

- This note: the condition's **derivation** — from the requirement, not from the symptom.
- `docs/solutions/best-practices/a-sentence-that-enumerates-a-set-is-a-claim-about-every-member-check-the-far-side-of-the-boundary-INFRA-249.md`:
  the same mistake made in prose, where a sentence enumerated the refusals on one side of a
  service boundary without following what the other side does to them.
- `docs/solutions/testing/a-check-is-proven-by-the-control-that-makes-it-fail-not-by-its-own-green-INFRA-250.md`
  (dispatch://INFRA-250, written at that issue's own retro): whether the check **can fail at
  all**. Empty is the limiting case of a failure set smaller than the requirement, which is why
  neither note is the other's cause.

dispatch://INFRA-246 reached the same shape from a third direction the same night — three
conditions inferring a value's provenance from live variables, replaced by stating what the value
is — but that issue was still in review rounds when this was written, so nothing of its conclusion
is asserted here. Read its own record before citing it.
