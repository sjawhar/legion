---
title: "A failed measurement read is unknown, never zero"
category: testing
tags:
  - measurement
  - load-test
  - sampler
  - metrics
date: 2026-10-28
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2369"
---

# A failed measurement read is unknown, never zero

A harness, probe or proof summary reports each number either as measured or as not measured. It
never reports a read that failed as 0. A 0 that came from a failed read looks like a result, and it
moves every statistic that includes it.

- **Mark the failed read where it happened.** Record which reads of a sample failed (for example
  `failed_reads`). Keep those reads out of every statistic, and count them.
- **Show an unread cell as `-` with a mark that says why.** Put a note under the table that names
  the reason, and the field in the result file that holds the error.
- **Fail the run when a count it depends on was not read.** If the committed rows or a delivery
  join could not be read, exit non-zero. A table row that only reads `0 committed` shows a load
  collapse that never happened.
- **Drop a sample taken after the run's context ended.** Its reads fail because the run is over,
  not because the system failed. If you keep it, it adds zeros, or a negative rate from a
  difference against 0.
- **Compute a rate only from two reads that worked.** A rate taken from a failed read is a large
  negative or positive number. When fewer than two reads worked, the rate is `null`.
- **Fail a run whose dependency process exited during it, and name the process.** Without this, the
  run writes a normal row for a stack that was no longer there.
- **Test the path with the dependency gone.** Point the probe at a closed port, or at a database
  without the table it reads. Assert the mark, the exit code and the absence of a 0. Run the
  previous code as the negative control.

## Evidence

In sjawhar/legion#2369 (`packages/envoy/loadtest/`), five review or test rounds each found a
failed read that came out as 0:

- `5ad8c796`: the 5 s sampler recorded 0 connections and 0 goroutines for a failed read.
- `9095579b`: a step whose Dispatch was killed exited 0 and wrote a normal row.
- `5edd1f88`: the NATS rates came from failed `/varz` reads.
- `a4e846c7` (the tester's red test) and `1e673f40`: a run against a database without the commit
  trigger showed `0 committed`, `0 / 0` receipts and empty latencies as measured. Now those cells
  show `-` and `(commit rows not read)` (`tables.go:152-181`), and the step exits 1.
- `f93dbc53`: a sample taken after the step ended entered the statistics as 0 connections and
  negative NATS rates. `sample.go:314-320` now drops such a sample.
