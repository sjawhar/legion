---
title: "A deadline that abandons without cancelling makes a serial queue concurrent"
category: architecture-patterns
tags:
  - concurrency
  - serial-queue
  - timeouts
  - shared-mutable-state
  - pi-envoy
date: 2026-10-25
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-246"
  - "sjawhar/legion#2164"
---

# A deadline that abandons without cancelling makes a serial queue concurrent

A serial queue is chosen so that each task sees the state the last one left. Giving its tasks a
deadline that rejects the caller's promise while the task keeps running takes that guarantee away
and gives nothing back in its place: two tasks now run over one set of mutable fields, and the
code still reads as serial. Before adding one:

- **Fix the symptom outside the queue first, and see what is left.** The lapse this change was
  filed for is cured by posting the periodic effect from the timer, outside both queues, whenever
  the last one is a whole interval old — which works whether or not the wedged task is ever
  abandoned. Decide what the deadline adds once that is in place, and write the answer down.
- **If a deadline is still wanted, prefer a shape that ends the task.** Abort the work (an
  `AbortSignal` the awaits honour), or end the process, or restart the queue. "Abandon and keep
  running" is the shape that costs a hand-written guard at every write after every await.
- **Price the guard before you choose it, in sites.** Count the awaits that precede a write of
  shared state; that count is the ongoing cost, paid by every future change to the file. Here it
  was about twenty, and six of the nine review rounds found a write that had been missed.
- **A guard a compiler cannot enforce is a convention, so say so where someone will read it.**
  One named predicate per fenced scope, defined at its top with the rule in a comment, is what
  survived review; three spellings of the same re-read did not.
- **Do not let a periodic effect that runs outside the queue write the queue's state unguarded.**
  A timer-driven writer is not covered by an audit of continuations inside queued tasks, and a
  map it mutates can be under iteration by a task that is still live.
- **Hold one standard for guards against unseen failures.** A clamp was declined in the same
  change as "a guard against a failure nobody has seen"; the deadline was built under the same
  justification and kept. Either reasoning can be right — but apply it once, to both, and record
  which way and why.

## Evidence

`packages/pi-envoy/extensions/envoy.ts`. The planner measured every await in the pass as already
bounded (HTTP carries `AbortSignal.timeout(5000)` with at most two attempts; the NATS client has a
20 s dial default) and recorded that the deadline was built "as a guard against a future await
rather than a fix for an existing one". A pane with NATS black-holed still registered three times
in 75 s.

Nine review rounds followed. Every product finding in six of them was a write after an await by a
task that had been abandoned or whose session had moved: the hold-settling loop (four rounds,
including one where the fix took a role from a live process in another pane), a claim the listener
granted to an abandoned task, the connection setup, a refusal record, and a settled-for marker.
The deadline was not deleted — a wedged task otherwise holds session events and claim calls behind
it, which the issue's acceptance asks to bound — but the choice between the two shapes was never
costed against the backstop-only alternative, and the convention it created is the first entry in
the pull request's own risk list.
