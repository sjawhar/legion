---
title: "A bound that guards a shared dependency is paid once per dependency, not once per call"
category: architecture-patterns
tags:
  - timeout
  - bound
  - shared-resource
  - nats
  - background-retry
  - startup-path
date: 2026-11-03
status: active
module: packages/pi-envoy
related_issues:
  - "sjawhar/legion#2699"
---

# A bound that guards a shared dependency is paid once per dependency, not once per call

Extends docs/solutions/architecture-patterns/work-detached-from-its-caller-needs-its-own-mark-deadline-freshness-and-waiter-pr2641.md,
which gives one detached flight its mark, deadline, freshness rule and waiter. This is the scale
rule under it: many calls, one dependency, one bound.

- Count the calls one pass makes against the dependency before you choose where the bound sits. A
  startup or reconcile pass that opens several things against one connection pays `bound × calls`
  when the bound is per call, and that product is what passes the pass's own deadline.
- Mark the dependency when a call goes unanswered within the bound, and let every later call on a
  marked dependency go on at once. One pass then waits once, whatever it opens.
- Name every event that clears the mark, and a cap. "It answered again" is not one event: the
  answer, a reconnect that re-establishes the dependency, and the owner shutting down each clear
  it, and a cap ends the asking so a dependency that never answers is not marked for the process's
  life. A mark nothing clears is worse than the per-call bound it replaced.
- Keep something asking. The call that gave up hands the question to a background retry whose only
  job is to clear the mark, so the next call is checked again rather than trusted forever. Nothing
  waits on that retry.
- Say which of two things the bound buys. Paying it once per dependency is not skipping it: a call
  the dependency never answered goes on unconfirmed, exactly as it did before the bound existed,
  and whatever that window loses is lost. Write that cost beside the rule, or a reader will take
  the mark for a guarantee.

## Evidence

In sjawhar/legion#2699 (INFRA-436) a subscription is confirmed by the NATS server before the
session is registered on it. The first shape bounded that round trip per open. A reconcile pass
opens the agent subject, the relay, the control subject and every recovered topic before it
registers, and a reconnecting nats.js connection answers no round trip and, with
`maxReconnectAttempts: -1`, is never closed: review round 4 measured 30.27 s of held HTTP
registration for three subjects, and a resume with six recovered topics would have passed the
pass's own 90 s deadline, where the session registers only through the heartbeat's stall backstop.

The fix is a `Set` of connections that went unanswered (`packages/pi-envoy/extensions/envoy.ts`,
`unanswered`), a return at the head of the confirmation while the mark stands, and three clears —
the background confirmation's `finally`, `Events.Reconnect`, and the session's shutdown — with a
ten-minute cap on the asking. The row `a pass that opens many subjects over a connection that
answers none waits one bound` takes 1 round trip where the per-open shape takes 9, and the whole
pass measured 10.30 s against 30.28 s.
