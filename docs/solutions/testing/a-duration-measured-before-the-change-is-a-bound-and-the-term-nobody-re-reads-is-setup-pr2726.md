---
title: "A duration measured before the change is a bound, and the term nobody re-reads is setup"
category: testing
tags:
  - ci
  - go-test-timeout
  - measurement
  - github-actions
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# A duration measured before the change is a bound, and the term nobody re-reads is setup

Extends docs/solutions/testing/a-race-lane-names-the-packages-that-hold-the-shared-state-and-its-job-limit-outlasts-every-steps-test-timeout-INFRA-141.md.

That note's third bullet is the budget rule: a job's `timeout-minutes` stays above the sum of the
largest setup time and, for each `go test` step, its largest measured green duration plus its own
`-timeout`.
Two things about the terms of that sum, which the rule does not say and which a later editor
needs.

- **A term measured before the change is a bound, not a measurement, whenever the change moves
  work between steps.**
  Label it as one in the comment that states the sum, with the reason, so the next reader does not
  take a number the change already made obsolete for a figure of the job as it now runs.
  A bound is the safe direction, so it needs no correcting — it needs saying.
- **Re-measure the terms your change did not touch.**
  Setup is the term nobody re-reads and the one that drifts: it is every step before the first
  `go test`, so an added install action or a slower cache restore moves it while all the attention
  is on the test steps.
  Read it at the head you are sizing, not only from the historic window, and spend the headroom
  the sum leaves only after you have.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) raised the `test` job's `timeout-minutes` to 80 for a
sum of 4578 s, about 76.3 minutes:
325 s of setup, the plain step's 839 s plus its `-timeout 15m`, the race step's 694 s plus its
`-timeout 30m`, 13 s for the steps after it and 7 s for the cache save.

- **The bound.** The plain step's 839 s was measured over 30 jobs in which that step still ran
  `internal/daemon`, the package the change moves out of it.
  At the pull request's own head the same step took 450 s.
  The reviewer asked for the term to be labelled, and the comment beside the limit now reads
  "839 s (measured while it still ran internal/daemon, so an upper bound for it now)".
- **The drift.** Setup in the sum is 325 s, the maximum over the same 30 main jobs.
  The pull request's own `test` jobs measured it at 330 s and then 371 s, 46 s above the figure
  the sum uses.
  Nothing fails — the criterion reads setup over that window, and even at 371 s the worst case is
  4624 s, 77.1 minutes, inside 80 — but it leaves 2.9 minutes of headroom rather than the 3.7 the
  comment implies, and the next change to the job's step list should re-measure before it spends
  any of it.
