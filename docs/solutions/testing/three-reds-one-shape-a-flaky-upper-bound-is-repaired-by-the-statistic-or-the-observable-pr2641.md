---
title: "Three reds, one shape: a flaky upper bound is repaired by the statistic or the observable, never by the bound"
category: testing
tags:
  - flaky-tests
  - bounds
  - timing
  - memory
  - mutation
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2641"
  - "sjawhar/legion#2665"
---

# Three reds, one shape: a flaky upper bound is repaired by the statistic or the observable, never by the bound

Extends docs/solutions/testing/a-row-that-fails-at-about-five-seconds-in-a-worker-pod-is-buns-spawnsync-timeout-under-gvisor-INFRA-92.md,
which says not to widen a bound and to measure the call rather than the harness. This one is about
the red that is neither the code nor simply the host, and about what to do instead of widening.

## The shape

One required CI run reddened three tests at once in packages one change did not touch. Each was
**one observation held under an upper bound**:

- a timing test asserting the gap between two log events was under the interval plus 100 ms;
- a memory test holding **one** run's peak resident memory to 256 MiB;
- a readiness test reading a durable consumer immediately after binding it.

None of the three is the code and none is answered by a bigger number. Each was repaired by
changing *what is observed* or *which statistic of it is held*:

- the timing test records **what the loop armed** and holds the gap to at least that, which a
  loaded machine only lengthens;
- the memory test holds the **least of three runs** to the bound, since uncollected garbage only
  adds to a peak (the same edit peaked from 190 to 263 MiB over five runs with a live heap under
  about 130 MiB);
- the readiness test **waits for the state production waits for**, the server's own report that
  the durable is bound.

## Rules

- **Before you widen a bound, name the mutation that bound was the only thing failing, and run it
  against the new one.** Widening `leaseAllowance` from 100 ms to `interval / 2` kept every honest
  run green and let a `Run` that ignored the jitter draw and waited `interval + interval/10` every
  period pass both rows, which is the behaviour the test is named for. A widening that no mutation
  contradicts is a deletion written as a number.
- **Three simultaneous reds in unrelated packages are evidence of a shared shape, not of a shared
  machine.** "The runner was loaded" explains one red and predicts nothing; "each of these holds a
  single observation under a ceiling" predicts the next one and names the repair.
- **A bound that is the *only* thing left is a hang guard, and it is judged by a different rule:**
  it must stand far from the work's own duration, print the bound it gave, and stay inside the
  step's own `-timeout` so a hang prints the dump that names it (`.github/workflows/ci.yaml:2563-2566`;
  the lane that runs this package is `go test -timeout 20m`, `ci.yaml:2390-2391`).
- **Read a package's `--- FAIL` count, never its exit status alone, before you call a local run
  red.** On a Legion pod's test Postgres a Go package exits 1 with every test passed, because
  `storetest` cannot drop its isolated databases
  (docs/solutions/testing/the-dispatch-suites-on-the-pods-test-postgres-report-fail-after-pass-because-the-test-role-cannot-drop-its-databases-pr2444.md).
  The grant that would fix it cannot be obtained in the pod: `grant pg_signal_backend to
  legion_test` answers `Only roles with the ADMIN option on role "pg_signal_backend" may grant
  this role`, so no round is worth spending on it.

## Evidence

- The run that reddened sjawhar/legion#2641's retro head `6116513158c5`: `sweep 2 listed
  1.221943264s after sweep 1 ended` in `packages/envoy/internal/dispatch/redeliver/sweep_lease_test.go`,
  `a cold one-word edit of a four-column table held 272 MiB above idle, want at most 256 MiB` in
  `packages/envoy/cmd/dispatch/edit_memory_test.go`, and
  `TestAShortLivedCredentialIsRefreshedBeforeItLapses: condition never became true` in
  `packages/envoy/internal/broker/helper/refresh_test.go`.
- sjawhar/legion#2665's body ("Three tests failed in this pull request's CI with the code
  unchanged, and each is fixed here in a commit of its own") holds the three repairs and the
  190–263 MiB measurement.
- The widening and its mutation: #2641's round 8 set `leaseAllowance = interval / 2`; the control
  `time.NewTimer(interval + interval/10)` in `Run` passed both rows under it and fails the
  sequence under the version that replaced it.
- The lease's own statements, the thing the 100 ms was for, cost 2.72 ms to 6.94 ms over twelve
  waits in six runs on an idle pod, against the 222 ms CI saw.
