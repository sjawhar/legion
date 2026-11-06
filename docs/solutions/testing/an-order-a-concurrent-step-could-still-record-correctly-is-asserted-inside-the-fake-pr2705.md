---
title: "An order a concurrent step could still record correctly is asserted inside the fake"
category: testing
tags:
  - call-order
  - recorded-sequence
  - fakes
  - go
date: 2026-11-04
status: active
module: packages/envoy/cmd/broker
related_issues:
  - "sjawhar/legion#2705"
---

# An order a concurrent step could still record correctly is asserted inside the fake

Extends docs/solutions/testing/prove-call-order-with-a-recorded-sequence-not-microtask-counting.md.

A recorded sequence proves the order the steps were recorded in. It does not prove that one step
had not started when another ran: a step launched on a goroutine of its own can finish early and
still leave the expected sequence behind, because the sequence records when each step completed,
not what was already running.

- For each ordering the sequence cannot show, assert it where the dependency lives: inside the
  fake of the later step, which fails the test the moment a step it must come before has already
  been recorded.
- Give the recorder a membership read (`has(step)`) beside its equality read (`is(steps…)`), and
  say in its comment which question each answers.
- Leave the sequence assertion in place as well: the fake catches a step that ran too early, the
  sequence catches one that ran in the wrong place.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454) extracted the secrets broker's boot and stop
sequences into `boot` and `stop` (`packages/envoy/cmd/broker/boot.go`) and pinned five orderings
in `cmd/broker/ordering_test.go`, each red on one named one-line revert.

The recorder there carries both reads, with the reason inline: `has` "reports whether step has
run, which a fake reads to fail the moment a step it depends on has already happened — a
dependency the recorded order alone cannot show, since a sequence that ran that step on a
goroutine of its own could still record the expected order."

The ordering it guards is the stop's: the serving context must still be live when the shutdown
returns. The row's `Shutdown` fake records that the shutdown began, then fails on
`ran.has(theServingEnded)` — "the stop ended the serving context before its shutdown returned:
the reread listener and the five-minute policy reload stop under requests the broker is still
answering" — because a stop that cancelled serving on a goroutine of its own, before or during
the shutdown, would leave the three records of that row in exactly the order it wants.
