---
title: "Filter a process-wide log capture by a client name the caller passes in, never by waiting for a record that names it"
category: testing
tags:
  - logging
  - flaky-tests
  - nats
  - test-helpers
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2506"
---

# Filter a process-wide log capture by a client name the caller passes in, never by waiting for a record that names it

Extends docs/solutions/testing/the-race-detector-runs-in-a-worker-pod-with-zig-as-the-c-compiler-and-a-library-callback-from-an-earlier-test-writes-your-log-sink-INFRA-182.md,
whose rule is already "keep only your own client's records (filter on the `name` attribute)". This
is how to hold that filter to the client under test without writing a second race.

- **Bind the name once at the call site and pass it to both sides**: `name := t.Name()`, then
  `bus.WithName(name)` on the client and `warnOrErrorRecords(t, logs, name)` on the assertion. A
  helper that derives the name itself cannot tell "this test named its client" from "this test
  named nothing", and silently drops the records it exists to read.
- **Never make the helper wait for, or require, a record that names the client.** A healthy client
  logs nothing under its name while it runs: the records that carry it are its close's, which the
  library reports from its own callbacks after the drain has returned. A guard that refuses its
  caller until such a record lands makes every caller race those callbacks.
- **Refuse a caller that names no client, and put the remedy in the message.** An empty name makes
  the filter drop every record that names one, the client under test's among them, so the
  assertion would pass while reading nothing.
- **Know what the filter does not cover.** Records that name no client are kept, because the
  records such a test asserts are its watcher's and its reconcile's, which carry no name. A leaked
  *watcher* of another test would still reach a "logged nothing" assertion; only named clients are
  separated.

## Evidence

- `packages/envoy/internal/bus/drain_kv_watcher_test.go:186-216` (the helper, its refusal and its
  filter) with the call sites at `:40-41`, `drain_hook_edges_test.go:88-89` and `:215-216`. The
  default client name is `"envoy"` (`internal/bus/nats.go:64`), which is why an unnamed client is
  indistinguishable from any other test's.
- The round that shipped the record-presence guard was followed by the round that took it out. The tester measured
  20 drains of `TestAStuckDeliveryStillEndsTheKVWatchers`' shape under `-race`: 4 had no record
  naming the client when `Drain` returned, the first landing 1.629 to 2.254 ms later, and that
  test reads the capture with one `ConsumerNames` round trip as its whole margin.
- The red test `TestWarnOrErrorRecordsTakesAClientThatHasLoggedNothing`
  (`drain_kv_watcher_test.go:152-174`) fails 5 of 5 runs with that guard restored and passes
  without it; a call site passing `""` fails with the refusal's own message.
- With the filter in place the three drain tests ran 0 of 100 failures under `-race`, where the
  same assertions caught another test's cluster client 11 of 20 iterations at the base.
