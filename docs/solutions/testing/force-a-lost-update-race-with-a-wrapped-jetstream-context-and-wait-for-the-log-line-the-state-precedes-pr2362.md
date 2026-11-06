---
title: "Force a lost-update race with a wrapped JetStream context, and wait for the log line the state precedes"
category: testing
tags:
  - race
  - jetstream
  - deterministic-tests
  - log-assertions
date: 2026-10-28
status: active
module: packages/envoy/internal/bus
---

# Force a lost-update race with a wrapped JetStream context, and wait for the log line the state precedes

- Test a lost update on a call with no compare-and-swap by forcing the losing order, not by
  starting two clients at once and hoping. Wrap `nats.JetStreamContext` by embedding it, and run
  the competing write from a `sync.Once` hook after the wrapped `StreamInfo` or before the wrapped
  `UpdateStream`. Then assert the repair on every run.
- Record the server's answer to the write the wrapper sends. When the server can leave it
  unanswered, a test that requires the repair to log must first check that the losing write was
  applied; otherwise check only the end state.
- When code logs a line after the state change it reports, an assertion on that line must wait for
  it with the same bound as the state wait. Reading the state as done and then reading the log
  once fails under load.
- Reproduce such a flake with several race-built test binaries at once (`go test -race -c`, then
  three or four processes with `GOMAXPROCS=2`): one process at a time hid it.

## Evidence

`TestTheKeeperEndsTheMixedSettingsRaceAtThreeCopiesTheByteLimitAndTheID` first started two clients
at once on five clusters; the race happened in about a third of the rounds, so the keeper often had
nothing to repair. `interleavedJetStream` in `packages/envoy/internal/bus/keeper_test.go` now forces
the default start's stale write after the setting start's raise on one cluster (commit
fcd91e242074). Under four parallel race processes it then failed 1 run in 40 with `the keeper's
lines restored ""`: the stream already read as repaired, but the keeper logs after its update
returns. `awaitRestoredLog` waits for the line (commit 7f536f31512a); 1,120 of 1,120 runs then
passed (sjawhar/legion#2362).
