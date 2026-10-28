---
title: "A gain smaller than the run-to-run spread is judged by the function's profile share"
category: testing
tags:
  - performance
  - pprof
  - load-test
  - measurement
date: 2026-10-28
status: active
module: packages/envoy/loadtest
---

# A gain smaller than the run-to-run spread is judged by the function's profile share

Extends docs/solutions/testing/measure-the-mechanism-not-the-symptom-count-the-work-the-system-does-not-do-INFRA-228.md.

## Rules

- **Fix the rule before the head runs.** Write down the inputs (same seed, `--rng-seed`, route mix,
  `--events 0` for a read cost), the run count per side, the discard rule (`box_busy_before`
  above 10% in `run.log`) and the sampling floor (rerun all runs longer if the base share under the
  changed caller samples less than about 0.2 s), then run every base run before any head run.
- **Compute the spread first.** Spread is the highest base run's cores less the lowest. When the
  expected gain in cores is smaller than the spread, a cores comparison cannot decide; judge the
  changed function's share of profile CPU instead.
- **Measure the share with every node counted.**
  `go tool pprof -top -nodecount=1000000 -nodefraction=0 -edgefraction=0 -focus='<fn>' cpu.pprof`
  and read `P` from the "Showing nodes accounting for X, P% of T total" line. With the default
  `-nodefraction` or a small `-nodecount` that line sums only the nodes still shown.
- **Derive the threshold in the load test's own units.** `G` is the median base share of the
  function under the caller you changed (add `-show_from='<caller>'`). The change passes when the
  median head share is at most the median base share less `G/2`. The mechanism's negative control
  is the caller-scoped share reading 0 in every head run.

## Evidence

sjawhar/legion#2429 (load test from #2369): base cores 0.1974/0.1988/0.1904 gave a spread of
0.0084 against an expected gain of 0.0021 cores (`G` = 1.05 share points), so only the share rule
could decide. The median `Client.Sessions` share fell from 1.31% to 0.28%, under the 0.785% limit,
and the share under `listAgents` was 0 in all three head runs (PR comment 6449285567). The plan's
first threshold was in production units the 2-vCPU load test cannot show.
