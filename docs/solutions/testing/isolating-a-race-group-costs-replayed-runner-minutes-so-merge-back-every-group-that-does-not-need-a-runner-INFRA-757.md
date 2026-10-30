---
title: "Isolating a race group costs replayed runner minutes, so merge back every group that does not need a runner"
category: testing
tags:
  - race-lane
  - ci-cost
  - path-filters
date: 2026-10-30
status: active
module: .github/workflows
related_issues:
  - "INFRA-757"
  - "sjawhar/legion#2517"
---

# Isolating a race group costs replayed runner minutes, so merge back every group that does not need a runner

Extends `docs/solutions/testing/a-shared-race-steps-per-package-seconds-carry-its-siblings-contention-so-split-on-the-share-and-measure-again-alone-pr2516.md`,
which says to decide a split on the package's share of its bar and to measure again alone. This is
the other side of that ledger.

**Price the split before you keep it.** A job of its own buys a package a runner's whole CPU and
costs one job-run on every head its filter selects. Replay a real window of pull-request heads
through the filters before and after, cost each new job at its own measured minutes, and put the
two totals in the pull request. Two filters that keep a wide root select nearly every head, so
each of them doubles that part of the bill.

**Then merge back every group whose share does not demand a runner.** A group measured well under
its bar alone gains nothing from isolation and costs its own selection; put it back with the
others and keep the runners for the groups the measurement says need one.

**Isolation, not sharding, is the first move.** Measure each group alone before paying for a
shard of test files: contention, not size, is often what put a package over its bar.

## Evidence (INFRA-757, sjawhar/legion#2517)

Six race binaries shared one four-vCPU runner, so no package's own cost could be read and a red
package ended the job before the ones after it printed a timing line. Measured alone,
`internal/dispatch/pmdoc` fell from 1563.552 s to 1023.519 s and `internal/dispatch/api` from
1099.487 s to 746.718 s, so neither needed a shard; the listener step printed a figure for the
first time anywhere.

The replay covered 1643 non-draft heads over three days, matched with picomatch's semantics as
`dorny/paths-filter` uses them and cross-checked head for head against picomatch itself. A
five-job lane would have cost **61,768 runner minutes** against the single job's 39,200, a factor
of 1.58. Merging the two cheap groups back — `internal/bus` at 26.6 % of its bar and
`cmd/listener` at 33.9 % — took it to **47,614**, a factor of 1.21, with no package left above its
bar. The set of heads that start some race job was 1225 before and 1225 after, and no head lost
coverage.
