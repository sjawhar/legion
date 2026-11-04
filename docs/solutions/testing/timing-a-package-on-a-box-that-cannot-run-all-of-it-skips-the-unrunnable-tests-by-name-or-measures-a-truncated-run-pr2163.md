---
title: "Timing a package on a box that cannot run all of it skips the unrunnable tests by name, or measures a truncated run"
category: testing
tags:
  - go-test
  - timing
  - pods
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# Timing a package on a box that cannot run all of it skips the unrunnable tests by name, or measures a truncated run

Extends docs/solutions/testing/the-pod-only-skip-list-is-re-derived-against-a-main-extract-and-the-daemon-package-needs-a-longer-timeout-INFRA-225.md.

- A test that panics ends the whole test binary, and Go runs top-level `t.Parallel()` tests only after every sequential top-level test has finished.
  So a panic anywhere in the package drops every parallel test, whatever its file's place in the order.

- When you compare wall times, pass the tests the box cannot run to `-test.skip` by name, and check that every run you compare reached the same tests:
  compare the counts of finished tests, and look for the tests you are measuring.

- A truncated run reports a smaller number rather than a red, so a timing taken without that check can look like an improvement.

## Evidence

In round 34 the race binaries of `internal/daemon` ran on an issue pod with no Docker.
`TestDaemonPermissionsOnRealServers` panicked with `rootless Docker not found` (and, once skipped, `TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved`), ending the binary.
Before the seeds were made parallel, the kept-decision tests ran early in file order and were timed.
After it, they were scheduled last and never ran, and the run read 390 s against `main`'s 439 s.
With the three Docker tests skipped, all three runs finished every test (645 on `main`, 671 at the head), and the comparison was 943 s for `main`, 1291 s sequential and 1154 s parallel.
