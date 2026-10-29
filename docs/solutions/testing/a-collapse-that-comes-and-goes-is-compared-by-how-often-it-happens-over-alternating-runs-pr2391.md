---
title: "A collapse that comes and goes is compared by how often it happens over alternating runs"
category: testing
tags:
  - load-test
  - measurement
  - noise
  - statistics
date: 2026-10-29
status: active
module: packages/envoy
---

# A collapse that comes and goes is compared by how often it happens over alternating runs

- On a load step near saturation, never call a regression from one before/after pair. A
  metastable collapse (latency up tenfold, a lock held almost always) can strike either build.
- Define the episode before running (for example request p90 over 2 s), alternate builds run by
  run on fresh copies of one seed, and compare episode counts per build with a one-sided Fisher
  exact test. Compare the mechanism per transaction (here, the hold time of the commit-order lock
  read from the Postgres statement log), not latency percentiles.
- Attach raw runs to the issue as each round finishes. A pod's scratch disk can be recreated, and
  a summary written afterwards from memory is weaker evidence than the runs.

## Evidence

Pull request sjawhar/legion#2391. The first round-2 pair on the committed load reproduction's 1x step showed the head at
request p90 2,331 ms against 356 ms. 26 alternating runs (artifact `bisect-findings-md` on the
issue) showed the commit-order lock held equally long per transaction (hold p50 61-71 ms before,
63-80 ms after) and one collapse episode in 12 runs of each build (Fisher p = 0.76), both about
52 s into the load. Round 2's raw runs were lost when the pod's scratch disk was recreated before
they were attached.
