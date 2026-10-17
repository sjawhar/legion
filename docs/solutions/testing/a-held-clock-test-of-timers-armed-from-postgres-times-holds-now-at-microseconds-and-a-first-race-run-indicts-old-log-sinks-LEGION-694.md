---
title: "A held clock that fires timers by exact duration must hold now at Postgres's microsecond precision when the code arms a timer from a stored time, and a package's first race-detector run indicts its old tests' log sinks, not the new goroutines"
category: testing
tags:
  - held-clock
  - fake-clock
  - race-detector
  - go-test
  - slog
  - bytes-buffer
  - outbox
  - deterministic-tests
date: 2026-10-16
status: active
module: packages/daemon
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A held clock that fires timers by exact duration must hold now at Postgres's microsecond precision when the code arms a timer from a stored time, and a package's first race-detector run indicts its old tests' log sinks, not the new goroutines

- `internal/daemon`'s `heldClock.fire(after)` runs the timers armed for exactly `after`. Code that
  arms a timer from a time it read back from Postgres (`AfterFunc(next_at.Sub(clock.Now()), …)`)
  arms it for the stored time minus the held time, and `timestamptz` keeps microseconds: a held
  clock at a nanosecond-precise `time.Now()` arms a duration a few hundred nanoseconds off the hour
  the test fires, and `fire(time.Hour)` runs nothing. Hold the clock at
  `time.Now().UTC().Truncate(time.Microsecond)` (the daemon tests' `microClock()`), and the duration
  the lane arms is exactly the one the test fires.
- Adding `go test -race` over a package for a new concurrency mechanism reports, first, the races
  that were already there: tests reading a plain `bytes.Buffer` log sink (`slog.New(slog.NewJSONHandler(&buf, nil))`)
  while a goroutine of the code under test still writes it. Audit every `bytes.Buffer` handed to a
  `slog` handler in the package's tests before the step lands: a read after the goroutine is joined
  is fine; a read inside a `testwait.Eventually` or before the join is the race. Replace those sinks
  with the package's mutex-guarded one (`syncBuffer` in `internal/daemon/controller_watch_test.go`),
  and only then read a green step as evidence about the new code.
- A worker pod has no cgo, so `-race` runs nowhere but CI: add the step for the packages the new
  goroutines live in (`internal/daemon`, `internal/record`), with their fakes holding a fake clock,
  so the run is deterministic and its first failures are findings rather than flakes.

## Evidence

LEGION-694 (sjawhar/legion#1999) tests the outbox's lanes on `heldClock`: a row written with
`next_at` an hour out must run after `clock.advance(time.Hour)` and `clock.fire(time.Hour)` and not
before (`TestARowDueLaterRunsWhenItsLanesTimerFires`, `packages/daemon/internal/daemon/outbox_lanes_test.go`).
The lane arms `clock.AfterFunc(at.Sub(now), …)` from the `next_at` it read back (`outbox_lanes.go`,
`runLane`), `heldClock.fire` matches a timer by its exact duration (`supervision_test.go`), and
`timestamptz` stores microseconds, so a held time at nanoseconds arms a duration off the hour by the
truncated nanoseconds and `fire(time.Hour)` would run nothing. Every lane test holds its clock
through `microClock()` (`outbox_lanes_test.go:263-267`), whose comment records that reason; the
two rewritten integration tests that first inlined the same expression were moved onto it by the
simplify pass.

The pull request's CI step `Test the outbox's lanes and store under the race detector`
(`.github/workflows/pr-and-main.yaml`, `go test -count=1 -race ./internal/daemon/ ./internal/record/`)
reported five races on its first run (38954475696), every one in a test older than the pull
request: `TestWorkflowBootLogsItsDependencyOrder` read `logged.String()` while `watchController`
still logged, `TestPollHoldReleaseWarnsWhenAHoldPersistsPastItsBound` read `logBuf.String()` inside
its `Eventually` while `pollHoldReleaseWith` logged, and the audit found the same shape in
`TestAKubernetesDaemonServesItsWorkerStreamOnTCPAndRunsNoHostPaneMachinery`. None was in
`outbox_lanes.go` or the store, whose shared state (the lane registry, `leaseRenewal`,
`heldClock.Now`) is mutex-guarded. Three one-token edits to `syncBuffer` made the step green
(38956917435).
