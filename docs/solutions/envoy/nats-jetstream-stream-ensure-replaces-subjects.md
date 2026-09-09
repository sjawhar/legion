---
title: "bus.Connect's stream-ensure replaces the subject list, not merges it"
category: envoy
tags:
  - envoy
  - nats
  - jetstream
  - deployment
  - rollout
date: 2026-09-09
status: active
module: envoy
problem_type: architecture_pattern
component: messaging
severity: high
applies_when:
  - Adding a new NATS subject to the shared JetStream stream that multiple binaries ensure on boot
  - Rolling out a change that touches packages/envoy/internal/bus/nats.go's streamSubjects list
  - Diagnosing why a subject that one service publishes to never reaches a subscriber after a deploy
---

# `bus.Connect`'s Stream-Ensure Replaces the Subject List, Not Merges It

## Context

Every binary that calls `bus.Connect` (the dispatch server, the listener, any future NATS
client in this monorepo) ensures the shared JetStream stream on boot using its own compiled
copy of `streamSubjects` (`packages/envoy/internal/bus/nats.go:20-23`). `ensureStreamWithConfig`
(`nats.go:308-332`) compares the deployed stream's `Subjects` to the caller's `cfg.Subjects`;
if they differ at all, it calls `js.UpdateStream(cfg)` with the caller's own list
(`nats.go:311,319`) — JetStream's `UpdateStream` sets the stream's `Subjects` field to exactly
what it is given. There is no union, append, or merge: whichever binary calls `ensureStream`
last during a rollout window installs *its own* subject list wholesale.

This repo added `notifications.dispatch.>` to `streamSubjects` for the native Dispatch
workspace (ruling R5 in
`.superpowers/sdd/2026-09-09-dispatch-native-workspace/rulings.md`). If the production
listener is still running an older image whose compiled `streamSubjects` predates that
addition and it restarts before its image is bumped, its boot call to `ensureStreamWithConfig`
sees `Subjects` differ (the deployed stream now has the extra subject, the old binary's `cfg`
does not) and overwrites the stream back to the shorter list — silently dropping
`notifications.dispatch.>` until a newer image restarts and re-adds it.

## Guidance

- **A deploy that adds a subject to `streamSubjects` must bump every binary that calls
  `bus.Connect` in the same rollout window**, not just the one that newly needs the subject.
  An older binary restarting during the window is not a hypothetical — restarts happen for
  unrelated reasons (OOM, node recycling, manual redeploys) and `ensureStreamWithConfig` runs
  on every one of them.
- Treat this as a ledgered risk, not a blocker, when the two binaries cannot be bumped
  atomically: the safety net is that the dispatch server re-ensures the subject on its own next
  boot (`nats.go:230-`), so an old listener's overwrite is self-healing the next time the
  *newer* binary restarts — it is a window of dropped delivery, not a permanent loss. Ledger it
  explicitly with the follow-up (this repo's example: "production Fargate listener image bump
  for the `notifications.dispatch.>` stream subject" tracked as a same-day infra follow-up).
- The outbox's `published_at` gate is the reason the dropped-delivery window does not lose
  data. `scanBatch` only sets `published_at` after a successful `Publish` call to the
  issue-topic subject (`packages/envoy/internal/dispatch/outbox/publisher.go:127-137`); its
  `select … where e.notify and e.published_at is null` (`publisher.go:86`) means an event whose
  publish attempt failed (subject not accepted, connection error, anything) simply stays
  unpublished and is retried by the next scan tick (`publisher.go:47,57-59`) or process
  restart. A route publish to `notifications.role.*`/`notifications.agent.*` is logged and
  never blocks this gate (`publisher.go:145-157` calls `publishRoute` only after the
  issue-topic publish already succeeded) — only the issue-topic publish gates the row. Events
  stall during the bad window; they are never silently dropped from the durable log.

## Why This Matters

Without this, a routine deploy that only *adds* a subject looks safe (it is additive, no
existing subject is removed) but is not safe against restart ordering, because the "ensure"
step is a blunt overwrite rather than a set union. The failure mode is silent — no error, no
log line different from a normal boot — until whoever depends on the new subject notices
nothing arrives.

## When to Apply

Any change to `packages/envoy/internal/bus/nats.go`'s `streamSubjects`, or to any other
JetStream stream this codebase ensures with the same replace-on-mismatch pattern
(`ensureStreamWithConfig`'s comparison in `nats.go:311`). Does not apply to consumers/filter
subjects created via `AddConsumer`/`UpdateConsumer`, which are scoped per-consumer, not shared
across binaries.

## Examples

Reproducing the mechanism read-only (no cluster needed):

```go
// packages/envoy/internal/bus/nats.go
var streamSubjects = []string{
    "notifications.agent.>",
    "notifications.dispatch.>", // <- added this deploy
    "notifications.github.>",
    ...
}
// ensureStreamWithConfig, on any boot where the deployed stream's Subjects
// still equals the *old* list: UpdateStream(cfg) with THIS binary's list —
// old binary => old list wins, wholesale.
```

## Related

- `.superpowers/sdd/2026-09-09-dispatch-native-workspace/rulings.md` R5, R25 — the rulings that
  named this risk and the outbox safety net during the native Dispatch workspace build
  (`sjawhar/legion#826`).
