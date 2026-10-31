---
title: "Measure what a probe costs on the box it measures before trusting its numbers"
category: testing
tags:
  - measurement
  - load-test
  - postgres
  - pg_locks
  - observer-effect
date: 2026-10-28
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2369"
---

# Measure what a probe costs on the box it measures before trusting its numbers

A probe that runs on the same machine as the system under load takes CPU and locks from that
system. A small machine is the most exposed. Before its numbers go into a before/after claim, run
the same step with the probe and without it, on the same build and seed, and compare the result
the claim is about (for example the committed rate).

- **Sample at the lowest rate that answers the question.** A sampler that lists waiters needs one
  read a second, not one every 20 ms.
- **Under load, do not call `pg_blocking_pids` from a sampler.** It takes exclusive access to the
  lock manager's shared state. Find each waiter's blocker in the same `pg_locks` read instead (a
  granted holder of the same lock).
- **Write the measured cost next to the interval.** Put it in the constant's comment and the README,
  with the date, the build and the two results, so a later change to the interval has a number to
  beat.
- **Measure each probe on its own.** A probe that has no with/without measurement on record is an
  unknown share of the run-to-run spread.

## Evidence

In sjawhar/legion#2369, the commit-order lock fix's ungranted-lock sampler was ported into the load test with a
20 ms interval. At 1x, with about 45 waiters, each read took 75-180 ms of one Postgres backend.
1x committed 0.29-0.82 events/s with the sampler, against 1.63-1.81 events/s without it, on the
same build and seed (2026-10-27). `a7b5f467` set the interval to one second. `44664fc1` and
`f8d7b4fa` replaced `pg_blocking_pids` with one `pg_locks` scan per sample. The measurement is in
`packages/envoy/loadtest/locksampler.go:38-41` and the README's lock-sampler section.

The commit-order lock probe (`lockprobe.go`) still reads `pg_locks` every 20 ms. No with/without
measurement of it is on record. [INFERENCE] Part of the 0.23-1.95 events/s spread of 1x runs on one
build may be that probe's cost.
