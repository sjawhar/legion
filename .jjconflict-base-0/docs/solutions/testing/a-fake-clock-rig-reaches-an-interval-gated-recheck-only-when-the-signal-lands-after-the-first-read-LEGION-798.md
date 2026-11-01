---
title: "A fake-clock rig reaches an interval-gated recheck only when the signal lands after the first read, so one row plants it late and advances the clock"
category: testing
tags:
  - table-driven-test
  - fake-clock
  - probe-image
  - failed-mount
  - recheck
  - sandbox-rig
date: 2026-10-19
status: active
module: packages/daemon/internal/runtime/sandbox
applies_when:
  - Adding a path to stuck, or any poll-loop verdict in the sandbox runtime, gated on `r.now().Sub(last) >= interval`
  - Writing a refusal table whose rows plant a Kubernetes event or status before ProbeImage is called
  - Judging as tester whether an implementer's green table exercised the ordering production takes
related_issues:
  - "LEGION-798"
  - "sjawhar/legion#2043"
---

# A fake-clock rig reaches an interval-gated recheck only when the signal lands after the first read, so one row plants it late and advances the clock

Extends docs/solutions/testing/a-refusal-tables-negative-row-passes-every-check-but-its-own-so-it-pins-the-message-not-the-judges-order-LEGION-629.md.
That document orders a table's rows against the judge; this one orders them against time.

- The sandbox rig's clock moves only when a test calls `(*rig).advance`. A verdict that reads once
  per interval (`if r.now().Sub(*last) < interval { return nil }`, `last` zero at the attempt's
  start) therefore reads exactly once per attempt in every row that plants its signal before
  `ProbeImage` runs: the recheck branch is never entered, and a green table proves nothing about it.
- Production always takes the other order: the daemon's store shows the pod bound, the first pass
  reads before the kubelet has tried the mount, and the kubelet's event lands seconds later. Write
  one row for that order: wait until the first read has happened (a `PrependReactor` on `list
  events` counting reads), then create the event, then `g.advance(interval)`, then nudge the wait
  loop with a no-op pod update rather than waiting on its tick.
- Assert the number of reads, not only the final error: `reads == 2` (the first pass and the one
  recheck) distinguishes the recheck from a path that reads on every pass or never again.
- Before you mark a recheck pinned, break it (`!last.IsZero()` in place of the interval comparison)
  and watch which tests go red. If none does, the ordering is unpinned whatever the table's size.

## Evidence

`stuck` (`packages/daemon/internal/runtime/sandbox/probe.go`) reads a scheduled Pending probe pod's
events at most every `mountRecheck` (15 s); `mountRead` is a zero `time.Time` per attempt, so the
first pass after `pod.Spec.NodeName` is set always reads. The implementer's
`TestProbeImageRefusesAnOperatorVolumeThePodCannotMount` (ten rows) and
`TestProbeImageRefusesAContainerTheKubeletCannotConfigure` (five) each create their `FailedMount`
event or waiting state before `g.probe(p)`; every refused row is decided on that first read. The
tester mutated the gate to `!mountRead.IsZero()` and all three mount tests, the providers one
included, stayed green. `TestProbeImageFindsAFailedMountOnTheRecheck` (`probe_test.go`, commit
`a2c061aa`) is the row that pins the production order: a goroutine waits for `eventReads != 0`,
sleeps a few passes, creates the event, calls `g.advance(mountRecheck)` and nudges the pod; the
test asserts the refusal, one Sandbox create and `eventReads == 2`. It is red under the mutation
and on the branch's base, green at the head.

The tester's independent smoke with the rig's clock set to the wall clock (`Options.Now =
time.Now`, the event created 3 s after `ProbeImage` started) refused at 15.5 s: the recheck, plus
the wait loop's tick, which is the latency the issue's acceptance ("within one poll interval")
names and which no fake-clock row can time.
