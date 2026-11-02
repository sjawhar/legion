---
title: "A test that watches a retrying producer repairs between two waits and reports the live cause"
category: testing
tags:
  - flaky-tests
  - go-test
  - background-connect
  - test-helpers
  - deadline-failures
  - envoy-bus
date: 2026-10-31
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "INFRA-841"
  - "sjawhar/legion#2580"
---

# A test that watches a retrying producer repairs between two waits and reports the live cause

Extends docs/solutions/testing/a-stream-consumer-ends-on-the-producers-terminal-set-and-its-deadline-names-the-last-observation.md.

That document is about a channel of verdicts. These five rules are the same discipline where the
producer's stream is a log it writes while it retries, and where the test itself repairs what the
producer is retrying.

- **A logged failure is terminal only if the producer says so.** Read the producer's reporting
  function before you key a wait on one of its lines. A producer that picks the level by how long
  it has been stalled writes its first line while it is still retrying, so a wait that ends there
  asserts a distinction the system never makes.
- **Check that the terminal line can occur inside your deadline, by arithmetic, before you write
  the wait.** When the line needs a grace the producer measures and your wait is shorter than that
  grace, the rule you just wrote can never fire. Either raise the bound and say why, or stop
  reading the log for finality and wait for the outcome instead.
- **Put the test's own repair between two waits, never inside one.** Wait for the failure, return
  to the test body, repair, then start a second wait. The repair then runs on the test goroutine,
  which a repair that touches a test harness must (Go forbids `t.Fatal*` off it), and it sits
  inside no deadline, so a slow repair cannot spend the budget of the wait that follows it. A
  repair run from inside a wait needs a callback, a latch and a deadline reset, and that reset
  races the producer's own backoff.
- **Bound the wait after a repair by the producer's maximum backoff, and export that bound.** A
  repair that lands just after a failed attempt waits the whole backoff out before the next
  attempt runs, so the post-repair wait is the ordinary wait plus that maximum. When the maximum
  is a literal inside the producer's function body, the test can only copy it: raising it then
  breaks nothing at compile time and nothing locally, and surfaces as a flake in a required check.
  Export it as a constant both sides name.
- **A deadline message reads the producer's state at the moment it gives up.** The first line in
  the log is the wrong thing to print after a repair: it always names the cause the test just
  fixed. Print what an accessor returns now, one a success clears.

Two more, for the assertions around such a wait:

- **Return the record's fields, not a flattened error.** A log-derived failure collapsed into one
  `error` loses which line the producer chose, which is usually the thing under test.
- **A check the wait above it already guarantees proves nothing.** Replace it with a stronger
  observable, then prove the replacement can fail by feeding it a value the system cannot reach.

## Evidence

sjawhar/legion#2580, INFRA-841. `startDispatchStream` in
`packages/envoy/cmd/dispatch/stream_replicas_test.go` ended on the first failed run of the bus's
reconnect hooks, closed the client and returned the logged error. The bus retries that run itself
(`internal/bus/recovery.go:86-97` starts `recover()` after a failed run), so a first reconcile that
failed once on a cluster failed the test instead of waiting for the repair.

The issue's own suggested fix — match the ERROR `envoy nats reconnect hooks have not succeeded`
and not the WARN — cannot fire: `reportHookFailure` picks the ERROR only at
`stall >= RepairGrace` (`internal/bus/recovery.go:251-265`, `internal/bus/nats.go:551`, a minute),
and the helper's wait is `reconcileWait = 30 * time.Second`, whose expiry is a `t.Fatalf`. The
planner measured both numbers before any code was written.

Round 1 ran the repair from a callback inside the wait and reset the deadline after it. Review
refused that: the reset is 30 s and `recoverUntilHealthy`'s backoff reaches
`const maxBackoff = 30 * time.Second` (`internal/bus/recovery.go:309`), so a repair landing just
after a failed attempt could fire the fatal just before the bus reconciled. Round 2 replaced it
with two single-purpose waits on one `dispatchStart` value, `awaitReconciled(t, within)` and
`awaitHookFailure(t)`, so the test reads connect, wait for the failure, `Servers[2].Start` plus
`AwaitReady`, wait for the reconcile. The restart is on the test goroutine, which
`testnats.Server.Start` and `Cluster.AwaitReady` require (`internal/testnats/nats.go:332-341`,
`cluster.go:146-160` call `t.Fatal*`), and inside no deadline: `AwaitReady`'s 15 s restart of a
member no peer has heard from, measured at one call in three, now costs the test's clock and no
bound. Take a member down and back with `Stop` plus `Start` plus `AwaitReady`, never `Restart`,
which rewrites the whole configuration file (`internal/testnats/nats.go:403-416`).

`repairedReconcileWait = reconcileWait + 30*time.Second` (`stream_replicas_test.go:43`) names
`maxBackoff` in a comment and copies its literal, because the constant is inside a function body
and is not exported. The shipped test finishes in 1.51-1.96 s, so no automated run approaches that
bound; if `maxBackoff` grows, nothing fails until a required check flakes.

Review round 2 found two more. `awaitReconciled`'s timeout printed `reconcileFailure`'s first
logged line, which after a repair is always the pre-repair cause; it now reads
`s.client.HookStall()` (`internal/bus/nats.go:560-568`), the stall and the error of the last run
that failed, which a successful run clears. And the test's last check,
`reconciledFor(resolveWith(t, overrides))` after `awaitReconciled`, could not fail, because the
wait returns only once that predicate holds; it is now
`testnats.AwaitStreamCopies(t, cluster.JetStream(t), bus.Stream, 3)`
(`internal/testnats/streams.go:57`), which also reads settled copies, and asking it for 4 copies
fails the test in 31.65 s, so the replacement is not vacuous.

`reconcileFailure` now returns `hookFailure{Message, Error}` with the two messages named once as
`retriedHookFailure` and `uncoveredHookFailure` (`stream_replicas_test.go:162-175`), in place of
`errors.New(refusal)`: the test asserts which of the bus's two lines it saw, which is what the
issue was about.
