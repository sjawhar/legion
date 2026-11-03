---
title: "A go test step with no -timeout gives the whole package ten minutes, so seeded tests run in parallel where each owns its state"
category: testing
tags:
  - go-test
  - race-detector
  - property-test
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# A go test step with no -timeout gives the whole package ten minutes, so seeded tests run in parallel where each owns its state

Extends docs/solutions/testing/a-new-tests-race-cost-is-charged-to-a-package-already-near-its-lanes-limit-so-size-the-fixture-by-the-cases-pr2497.md.

- A CI step that runs `go test` with no `-timeout` still has one: go test's 600 s default, for each package as a whole.
  Before you add slow tests to a package, read its race step and compare the base's measured time for that package against that limit.

- Measure base against head on one machine, with each test's own time (`go tool test2json`, or `-v`), and name the tests that make up the difference.

- When the cases cannot be made cheaper (a property model's seeds are the cases), run them with `t.Parallel()`, but only where each run already owns its state:
  its own database, runtime and directories.
  Total any shared counters under a lock, after a group subtest that returns only when every seed has.

- Run each seed once: a seed two tests both run at the same length and checks is one run too many.

## Evidence

At `6b608270` the daemon's race step (`go test -count=1 -race ./internal/daemon/ …`, no `-timeout`) timed out at 600.171 s in `internal/daemon` (run 41563842147), where `main`'s own race run took 340-434 s.
On one 2-CPU pod, under the race detector, the package took 943 s on `main` and 1291 s at the head.
Three seeded tests in `packages/daemon/internal/daemon/kept_decision_*_test.go` made almost all of the difference (170, 84 and 16.5 s).
Each seed's run builds its own database, runtime and state directory (`newKeptRun`), so `7f427f71` made the seeds and the three tests `t.Parallel()`, totalled the property test's counts under a mutex, and ran the three seeds two tests shared only once.
The package took 1154 s, with no seed or check dropped and no data race; the next CI run passed with `internal/daemon` at 563.8 s under race.
`main` then gave the step `-timeout 30m` itself (#2709).
