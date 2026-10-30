---
title: "Pin a settle-from-state loop with a scripted fake that counts its sends and its reads"
category: testing
tags:
  - test-doubles
  - retries
  - nats
  - jetstream
  - mutation-testing
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2506"
---

# Pin a settle-from-state loop with a scripted fake that counts its sends and its reads

Extends docs/solutions/testing/force-a-lost-update-race-with-a-wrapped-jetstream-context-and-wait-for-the-log-line-the-state-precedes-pr2362.md,
which wraps a *real* JetStream context to force an order. When the loop under test only sends,
reads back and decides, no server is needed at all.

- **Script the answers.** Embed the interface, override the two calls the loop makes, and replay
  one answer per send, `nil` once the answers run out. Apply the configuration to the fake's held
  state when the answer is `nil`, and answer every read from that state.
- **Model the other writer with one flag that applies the update it was sent**, not a literal
  field value: `landed` then means "the state now holds what this caller asked for", whatever the
  row asks for, and the fake does not quietly agree with one row's assertion by coincidence.
- **Every row pins the verdict, the sends and the reads.** A table that pins the verdict alone
  passes the mutant that settles *every* refusal from the state: the verdicts stay right wherever
  a later send succeeds, and only the counts show the extra read.
- **Include the row where the state holds the update but the refusal is not the settleable one.**
  That is the row that fails the same mutant on its verdict, and the one a verdict-only table
  cannot reach, because the common case has the state not holding the update.
- **Pair the deterministic table with the probabilistic test on the real thing, and say in each
  comment which guards what.** The cluster test meets the server's race about 1 in 900 sends, so
  it catches a regression only sometimes; the table fails every run. A reader who does not know
  that will trust the wrong one.

## Evidence

- `packages/envoy/internal/bus/stream_update_overlap_test.go:65-100` is the fake
  (`scriptedUpdateJetStream`: `answers`, `landed`, `sent`, `reads`), `:21-63` the eight-row table,
  `:101-148` the 300-round cluster test whose rounds are cheap because none restarts the cluster.
- The mutant `updateStream` that settles every refusal from the stream: round 1's verdict-only
  table passed it; with the counts and the new row it fails on
  `sent 1 updates and read the stream 1 times, want 1 and 0` and on
  `updateStream = <nil>, want nats: insufficient resources`.
- The same table caught the base's behaviour on 4 of its 8 rows, and the cluster test failed 4 of
  5 runs at the base against 0 of 10 at the head under `-race`.
