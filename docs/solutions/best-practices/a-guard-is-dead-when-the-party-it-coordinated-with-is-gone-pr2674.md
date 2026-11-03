---
title: "A guard is dead when the party it coordinated with is gone: count the parties, never read the guard's shape"
category: best-practices
tags:
  - deletion
  - dead-code
  - concurrency
  - simplify-pass
  - code-review
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2674"
---

# A guard is dead when the party it coordinated with is gone

Extends docs/solutions/legion/a-component-that-loses-one-of-two-callers-is-re-read-against-what-the-survivors-data-source-can-produce-pr2649.md.

## The rule

When a deletion removes one of the parties a mutex, a once-flag or a latch coordinated, count the
parties again before you keep the guard.
One party left means dead code, however defensive the guard reads.

Prove it by enumerating every call site of the function the guard protects, and showing that each
path runs it once and returns.
Put that count in the review thread or the handoff.
"It looks safe" is not the finding; "fourteen call sites, each followed by a return, one message
per handler goroutine" is.

Read `skill://ce-simplify-code`'s "Never simplify away a safety check" for what it covers: a
trust-boundary validation, a data-loss protection, a security check, an accessibility affordance.
It does not cover a coordination guard whose other side went with the deleted path.
A pass that judges such a guard by its shape — a mutex, a `settled` bool, the words "settle once" —
skips exactly the one this rule is about, because the shape of a dead guard and a live one is the
same.

## Evidence

sjawhar/legion#2674 deleted the Envoy listener's push of a message to a session's serve port.
`durableSettle` in `packages/envoy/cmd/listener/delivery.go` — a mutex plus a `settled` flag with
`end`, `retryAfter` and `finish` — existed so that the durable handler and each push it handed to a
per-session worker could settle one JetStream message once.
`cmd/listener/serve_port_workers.go`, deleted whole in the same change, was that other party.

The implementer's `ce-simplify-code` pass read the guard's shape and kept it, recording under
*Skipped*: "deleting `durableSettle` (its once-only guard is a safety check against a double
acknowledgement)".
The pass's own efficiency reviewer argued the same way, for the cost rather than the risk.

The reviewer then counted the parties instead.
With the workers gone the handler is the only one: one `deliveryMessage` is built per NATS message
in `jetStreamDeliveryHandler`, it is passed by value into one handler call, and every one of the
fourteen `finalize`/`finalizeAfter` call sites is either followed by a `return` or is its handler's
last statement — including the role forward the handler hands over, which carries
`roleForward{item, sessionID}` and never sees the message.
Nothing could reach one message twice.
`finalize` and `finalizeAfter` now call `message.msg.Ack()` and `NakWithDelay` directly, behind the
unchanged `mode != durableDelivery` guards, as they did before the workers were added.
