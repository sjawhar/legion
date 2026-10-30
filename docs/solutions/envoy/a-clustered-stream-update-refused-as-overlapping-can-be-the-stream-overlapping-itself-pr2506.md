---
title: "A clustered stream update refused as overlapping an existing stream can be the stream overlapping itself"
category: envoy
tags:
  - nats
  - jetstream
  - stream-update
  - concurrency
  - server-versions
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2506"
---

# A clustered stream update refused as overlapping an existing stream can be the stream overlapping itself

Extends docs/solutions/envoy/a-jetstream-answer-is-not-the-outcome-a-sequence-zero-duplicate-stored-nothing-and-a-timed-out-update-may-have-applied-pr2362.md.

- **A stream update refused `subjects overlap with an existing stream` (error code 10065) on a
  clustered server may be the other writer's doing, not yours.** Read the stream again: when it
  holds what the caller asked for, the update is done; when it does not, send the same update
  again, bounded. Only a refusal that survives the bound means another stream really overlaps.
- **Classify the answer before you choose the remedy, because the two wrong answers to one update
  need opposite ones.** An update whose answer never came may still have landed, so read back once
  and never send it again. An overlap-refused update wrote nothing, so sending it again is the
  write a server without the race would have taken after the other writer's. Every other refusal
  returns on the first send, with no read. Folding the two — settling every refusal from the
  state — leaves the verdicts right in the common cases and is still a defect.
- **Key the code on the error code only to decide whether to look again, never to decide the
  outcome.** The verdict comes from the caller's own requirement predicate over the re-read state
  (`holds(*nats.StreamInfo)`), so a requirement that never holds returns the server's own refusal.
  That is the exception to
  docs/solutions/architecture-patterns/derive-a-refusals-condition-from-what-the-caller-depends-on-not-from-the-mechanism-that-broke-it-INFRA-249.md,
  which otherwise forbids conditioning on the mechanism that broke.
- **State the bound in contenders, and state what it costs the path that legitimately fails.**
  Five sends outlast four concurrent updates landing in a row; a stream another stream does
  overlap pays four sends and five stream reads more before its refusal is returned.
- **Accept the widened window knowingly.** A re-sent update is the caller's configuration built
  from its first read, so a concurrent writer's larger byte limit or other subject list can be
  overwritten — now up to the bound's sends rather than none, repaired by the losing program's
  keeper within one period.
- **Name the upstream fix and the version that carries it; that is the loop's deletion
  condition.** nats-server commit `cb1afd68d014`, first released in v2.12.5, skips the stream by
  name instead of by pointer; every v2.10 and v2.11 release is exposed.
- **CI's pinned server can be older than the deployment's, so "production is unaffected" is a
  measurement, not an inference.** Drive the same contention against real binaries of the version
  the deployment runs before you write that sentence.

## Evidence

- The server reads the stream's assignment into `osa`, drops `js.mu` to run `configUpdateCheck`,
  retakes it, and calls `cc.subjectsOverlap(acc, cfg.Subjects, osa)`, which skips a stream only on
  pointer identity (`sa == osa`). An update that lands in between replaces that assignment, so the
  skip misses and the stream collides with itself (v2.10.29
  `server/jetstream_cluster.go:6280` and `:560-576`).
- `updateStream` (`packages/envoy/internal/bus/stream.go:505-563`) carries the rule for the stream
  and, through `raiseReplicas` (`kv.go:81`), for every bucket with no code change of its own.
  `overlapSends = 5` and `jsStreamSubjectOverlap = 10065`, which nats.go names no constant for.
- Measured on a real three-server v2.10.29 cluster (sjawhar/legion#2506): three clients sending one stream
  update at once refused 1 of 900 requests in 6.6 s. The acceptance test that failed in CI
  (`TestTwoStartsRaisingOneStreamAtOnceBothSucceed`) reproduced in 0 of 200 iterations in the same
  pod, so the direct probe is what named the cause.
- The same 900 updates against three real nats-server v2.15.1 binaries gave 0 refusals, before and
  after the fix: the hub's server carries `cb1afd68d014`, so the deployment never had this race
  while CI's pin did.
