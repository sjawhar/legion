---
title: "A local CPU figure belongs to the box and the hour, so only runs interleaved in one session compare"
category: testing
tags:
  - load-test
  - measurement
  - cpu
  - before-and-after
  - legion-pod
date: 2026-10-30
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2466"
---

# A local CPU figure belongs to the box and the hour, so only runs interleaved in one session compare

- **Never compare a CPU, latency or throughput figure measured in one session with one measured in
  another.** A fixed seed, fixed inputs, a pinned `GOMAXPROCS` and a private stack still leave the
  machine itself as a variable, and on a shared Legion pod it moves by more than most changes save.
- **Run base and head interleaved on one box in one session**: base, head, base, head, base, head.
  Never three of one build and then three of the other: a drift that starts between the blocks
  reads as the change.
- **Report an absolute figure with the time of each run's window**, and say that it depends on the
  machine and the hour. A single number with no times invites a reader to compare it with their own.
- **Prove "same work" separately from "same cost"** (a digest of the end state, equal across the
  runs of one build), so a difference in cost cannot be a difference in what was sent.
- When a run is repeated later, quote both sets with their times rather than replacing the old one.

## Evidence

`packages/envoy/loadtest/README.md:1034-1039` ("Measuring a change") states the rule as the load
test applies it.

The same Dispatch binary (35efd326), the same seed, the same inputs (rate 0.05, one read per write,
upload share 0.25, rng seed 1, `GOMAXPROCS` 2) and the same work (equal per-document versions,
sizes and anchors at the end) on the same 2-core pod type gave 9.60, 9.94 and 9.98 cores at the
full rate between 23:51Z and 01:53Z, and 14.65, 14.88, 14.76 and 14.74 cores between 17:38Z and
00:45Z the next day. The first ten minutes of the window cost 190-214 Dispatch CPU-seconds at
night and 271-280 in the afternoon. The first report quoted only the night's figure as Dispatch's
cost; the tester could not reproduce it, and the fix was to report both sets with their times.

The same drift misled the first reading of the upload share: a U = 0 and a U = 0.5 run from
midday compared with the night's U = 0.25 runs looked non-monotonic (13.1, 9.8, 15.9 cores).
Runs of all three shares in one evening session gave 12.73, 14.74-14.76 and 15.53: monotonic.
