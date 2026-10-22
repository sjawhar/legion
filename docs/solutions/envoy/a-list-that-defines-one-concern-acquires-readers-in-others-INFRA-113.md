---
title: "streamSubjects now decides transport as well as retention, and the routing predicate reads the compiled list"
category: envoy
tags:
  - envoy
  - nats
  - jetstream
  - rollout
  - skew
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-113"
  - "sjawhar/legion#2105"
---

# `streamSubjects` now decides transport as well as retention

Extends docs/solutions/envoy/nats-jetstream-stream-ensure-only-adds-subjects.md.

That page's "**adding a subject needs nothing special** once every writer runs a binary with this
fix" is still true of the stream, and is no longer true of the system. Add-only reconciliation
makes stream *membership* safe and says nothing about *transport*, because `streamSubjects` has
acquired a second reader since that page was written.

The delta, for anyone about to edit that list:

- A publish's transport is decided by `bus.usesCoreTransport`, which judges capture from
  `streamSubjects` **as this binary compiles it**, not from the deployed stream
  (`packages/envoy/internal/bus/nats.go`, `packages/envoy/internal/bus/stream.go`'s
  `streamCaptures`). Reconciliation is add-only, so while two builds differ the deployed stream is
  the union and no binary's view of it is the stream.
- That produces a third skew state the page does not anticipate. After a binary that adds
  `notifications.foo.>` starts, the deployed stream captures it; an older binary still sends a
  `notifications.foo.*` topic over **core NATS**, so the stream stores it without a `MsgId` (no
  duplicate suppression) and the older listener's durable, filtered to its own
  `bus.StreamSubjects()` (`packages/envoy/cmd/listener/durable.go`), never fans it out. The message
  is retained-without-dedupe and not-delivered at the same time.
- So a subject addition is a rollout with a skew window for delivery, not only for retention. Move
  every writer and every listener in one rollout, as that page already says for a *reshape*.

Name every reader at the list. `streamSubjects` has now been edited twice by someone reading only
its retention meaning, and both times a neighbouring concern silently changed:
docs/solutions/architecture-patterns/a-grant-built-from-a-neighbouring-concerns-list-inherits-every-narrowing-of-it-INFRA-113.md
is the first (the hub's publish grant); this is the second.

## Evidence

INFRA-113. The 2026-08 narrowing of `streamSubjects` from `notifications.>` to seven named
prefixes left every publish outside those prefixes going to JetStream, which never acknowledges a
subject no stream captures, and simultaneously narrowed the hub listener's rendered publish grant,
which is built from the same call. The fix added `streamCaptures` and the core-transport clause,
and in doing so made the list a routing input in its own right — which is what this page records.

The compiled-not-deployed choice is deliberate and documented at `usesCoreTransport`: a listener
cannot read the deployed subject list on every publish, and reading it once would be wrong after
the next deployment's start. The cost is the skew window above, which is bounded by the rollout.
