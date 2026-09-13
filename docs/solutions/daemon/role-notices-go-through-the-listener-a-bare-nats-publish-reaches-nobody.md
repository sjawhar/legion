---
title: "Role notices go through the listener's publish API: a bare nats.publish on a role subject is an invalid envelope and reaches nobody"
category: daemon
tags:
  - envoy
  - role-topics
  - nats
  - envelope
  - publishRole
  - envoyPublish
  - worker-queued
  - worker-started
  - silent-failure
date: 2026-09-13
status: active
module: packages/daemon
problem_type: correctness
severity: high
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
  - "LEGION-10"
symptoms:
  - "an architect told `worker-queued` never receives the `worker-started` it is told to wait for"
  - "`worker-died` / `launch-failed` / `revive-failed` never arrive at the architect or controller"
  - "the Envoy listener logs `listener invalid envelope: event_id is required` at the moment the daemon publishes a notice"
applies_when:
  - Adding or auditing any publish the daemon itself makes to a `notifications.role.<token>` topic
  - A role holder is not reacting to a daemon notice and the daemon's own log shows the publish happened
---

# Role notices go through the listener's publish API

## What was wrong, for months

`ProcessManager` published its own architect and controller notices — `worker-queued`,
`worker-started`, `worker-died`, `launch-failed`, `revive-failed`, and a redelivered exception
payload — with a bare `nats.publish(subject, json)` on the role subject. The Envoy listener validates
every role-lane message as an **envelope** (`event_id`, `source`, `topic`, `payload_summary`, …) and
drops a bare JSON payload with `listener invalid envelope: event_id is required`. None of those
notices ever reached a holder. Every other daemon notice (`phase-complete`, the catch-ups, `resync`)
already travelled through the listener and was fine — the bug hid in one dependency used by one
class.

It surfaced only because the LEGION-60 smoke rig had a real listener in the loop: at the exact
second the daemon committed a promoted prompt, `listener.log` showed the rejection, and the
architect's transcript showed no `worker-started`. Unit tests could not have caught it — the fake
`natsPublish` recorded the JSON and every assertion on "what was published" passed.

## The rule

A notice to a role topic is sent with the listener's `POST /v1/messages/publish` (`envoyPublish` in
`index.ts`), which wraps the JSON in an envelope and routes it to the live holder — never with
`nats.publish` on the subject. In `ProcessManager` the dependency is `publishRole` (`ProcessManagerDeps`
in `processes.ts`), wired to `envoyPublish` fire-and-forget with a logged failure; its doc comment
states this rule and why. The daemon `AGENTS.md` invariants bullet on role lanes names every
notice this covers.

Two facts worth keeping straight:

- The listener answers 404 for a role with no live holder. A notice that misses because the holder
  is down is recovered by the holder's later state-derived catch-up **only for the state the catch-up
  carries** — `overseerCatchup` carries no worker-promotion state, so a missed `worker-started` is
  recovered indirectly, by the worker's own later `phase-complete`, not by replay.
- `natsRequest` (the controller directive round-trip) is a different lane and is correctly a raw
  NATS request; the rule is about publishes to role topics.

## How to lock it

`index.test.ts`'s boot-redrive case asserts the architect's `worker-started` arrives through the
injected `envoyPublish` (topic and exact payload), which is what a consumer observes — not that some
`natsPublish` spy was called. Any new daemon notice should be asserted the same way.

## How it was found, and what that says about proof

The plan called for a smoke-rig round with a real listener as the implementer's own production-like
proof (deployment instructions, Sami 2026-09-13: the agent that developed it tests it before it
reaches production). That round found a second bug the issue had not named. A publish that "went
out" according to the daemon and "never arrived" according to the consumer is invisible to any test
that fakes the transport; the listener's rejection log is the instrument. See
`docs/solutions/testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md`.
