---
title: "Correcting one clause of a published sentence re-derives every clause of the sentence you write"
category: best-practices
tags:
  - documentation
  - operator-facing
  - settings
  - review-rounds
date: 2026-10-31
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "INFRA-841"
  - "sjawhar/legion#2580"
---

# Correcting one clause of a published sentence re-derives every clause of the sentence you write

- A fix to one false clause is a rewrite of the whole sentence. Check every clause of the
  sentence you write against the code, including the clauses you carried over, because a reader
  takes the corrected sentence as newly verified.
- Carrying a clause is the dangerous part: the clause you did not come to fix is the one nobody
  reads again, and a clause you add to set up the fix is read by nobody but you.
- An operator-facing string is published by the binary that prints it and by whatever generates
  documentation from that output, so its wrong clause reaches a reader without a diff. Name the
  surface in the proof and print it: a grep of the source does not show what the operator sees.

## Evidence

sjawhar/legion#2580, INFRA-841, item 2. The `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES`
rows of `packages/envoy/cmd/dispatch/settings.go` divided the WARN from the ERROR by "while the
retries stay under a minute", which is not the bound the guard enforces. The fix rewrote that
clause and, to introduce it, added a new one: Dispatch "connects in the background and reconciles
the stream on each connection". That second clause was false too. `ensureStreamOnce`
(`internal/bus/nats.go:496-508`) holds an `ensured` flag and returns nil on every connection after
the first success, and `packages/envoy/cmd/dispatch/AGENTS.md` already said "on its first
connection". The reviewer caught it and fixed both rows itself in `4430e212`; the shipped rows say
"reconciles the stream on its first connection, retrying until that reconcile succeeds".

The same round's other correction went the same way and survived: "the minute runs from the
connection event the reconcile has not covered" became "from Dispatch's first connection to NATS",
because `markHooksDueLocked` keeps the first uncovered event's time
(`internal/bus/nats.go:584-589`) and the reconcile is Dispatch's only hook, so for this binary that
event is its first connection. The first wording let an operator read each reconnect as restarting
the minute.

Both rows are published: `go run ./cmd/dispatch settings` prints them, and
`docs/site/generators/dispatch-config.ts` renders each as one markdown cell of the configuration
reference. Each round's proof ran that command and the docs generator rather than reading the
source.
