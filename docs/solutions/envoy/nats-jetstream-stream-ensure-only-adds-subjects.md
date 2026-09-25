---
title: "bus.Connect's stream-ensure keeps other deployments' subjects; retiring one is an operator step"
category: envoy
tags:
  - envoy
  - nats
  - jetstream
  - deployment
  - rollout
date: 2026-09-24
status: active
module: envoy
problem_type: architecture_pattern
component: messaging
severity: high
applies_when:
  - Adding or retiring a NATS subject of the shared ENVOY_NOTIFICATIONS stream
  - Rolling out a change that touches packages/envoy/internal/bus/nats.go's streamSubjects list
  - Diagnosing why a subject that one service publishes to answers "nats: no response from stream"
---

# `bus.Connect`'s Stream-Ensure Keeps Other Deployments' Subjects

## Context

Every binary that calls `bus.Connect` ensures the shared `ENVOY_NOTIFICATIONS` stream when it
starts, using its own compiled `streamSubjects` (`packages/envoy/internal/bus/nats.go`): the
listener (`cmd/listener`), Dispatch (`cmd/dispatch`), `natstail` (`cmd/natstail`) and the MCP
server (`cmd/mcp`). Several deployments of them write production's one stream and roll
separately: the production listener (applied by the production chain), native Dispatch (applied
by its own manual workflow), the on-prem Envoy fleet's listeners, which reach production's NATS
over Tailscale on their own image tag, and any ad-hoc `natstail`, MCP server or Dispatch run
pointed at production. A rollback, or a restart in the middle of a rollout, starts a binary
compiled with a different subject list from the one another live deployment was compiled with.

`ensureStreamWithConfig` therefore keeps the deployed subjects and appends each of the starting
binary's subjects the stream lacks (`reconciledSubjects`). It removes a deployed subject in two
cases only:

- the subject captures the role lanes, which travel over core NATS and must never be retained
  (`migrateRoleLanesOffStream`);
- the subject overlaps one of the starting binary's own subjects without equalling it (a
  widened, narrowed or split subject such as `notifications.legion.>` against
  `notifications.legion.*.*`). JetStream refuses two overlapping subjects in one stream, so
  keeping both would fail the start. The starting binary's shape wins, it logs one
  `envoy nats stream subject replaced by an overlapping one` line naming the dropped and the
  kept subject, and the next start of a binary with the other shape puts that one back.

`MaxAge` and the duplicate window still take the starting binary's values.

An earlier version of this page described the opposite behaviour, which was true until
sjawhar/legion's fix for LEGION-208 Stage 3: `ensureStreamWithConfig` called `UpdateStream` with
the starting binary's list whenever the deployed list differed, so whichever binary started last
installed its own list. On 2026-09-24 a local boot of both production digests showed the cost: a
Dispatch task on the previous image, restarted after the new listener, took
`notifications.legion.>` back out of the stream, and a Legion notice published through the
listener then answered `500 {"error":"nats: no response from stream"}`.

## Guidance

- **Adding a subject needs nothing special once every writer runs a binary with this fix.**
  Deploy the binary that adds it; a writer compiled before it that restarts keeps the new subject.
  The protection holds only after every writer runs an image carrying the add-only
  reconciliation: a binary built before it still replaces the list, so the first rollout of this
  fix must still move the listener, Dispatch and the on-prem fleet together.
- **Reshaping a subject (widening, narrowing, splitting) is safe to start but not to skew.**
  Every start succeeds, but while writers disagree on the shape, each start installs its own: a
  publish that only the other shape covers answers `no response from stream` until a binary with
  that shape starts again. Move every writer to the new shape in one rollout.
- **Retiring a subject is an operator step.** Remove it from `streamSubjects`, wait until no
  deployment compiled with it can start again (every writer moved past it, rollback anchors
  included), then edit the live stream by hand: `nats stream edit ENVOY_NOTIFICATIONS
  --subjects=<the list without it> -f`. Start-up never does it for you.
- **Two starts in the same instant can still lose a subject.** JetStream's stream update has no
  compare-and-swap: if two writers with different lists both read the stream before either
  writes, the second write omits the first writer's addition. It takes two process starts within
  one read-update round trip; the next start of the writer that lost its subject puts it back.
- The outbox's `published_at` gate is why a missing subject stalls Dispatch events rather than
  losing them: `scanBatch` sets `published_at` only after a successful publish to the issue-topic
  subject (`packages/envoy/internal/dispatch/outbox/publisher.go`), and a failed publish schedules
  a retry with backoff (`scheduleRetry`).

## Why This Matters

A deploy that only adds a subject looks safe, and with replace-on-mismatch it was not: the failure
was silent, with no error and no log line different from a normal start, until whoever depended on
the subject noticed nothing arrived. Add-only reconciliation makes the safe-looking deploy safe and
turns the one destructive change, retiring a subject, into a deliberate act.

## When to Apply

Any change to `streamSubjects`, to `ensureStreamWithConfig`, or to any other JetStream stream this
codebase ensures from more than one binary. It does not apply to consumer filter subjects
(`AddConsumer`/`UpdateConsumer`), which each consumer owns.

## Related

- `packages/envoy/internal/bus/stream_config_test.go`
  `TestConnectKeepsTheSubjectsAnotherDeploymentOfTheStreamNeeds`: a real NATS, a second deployment
  with a different subject list, both starting in turn.
- `sjawhar/legion#826`, which first named this risk when `notifications.dispatch.>` was added.
