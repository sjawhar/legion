---
title: "A race lane names the packages that hold the shared state, and its job limit outlasts every step's test timeout"
category: testing
tags:
  - race-detector
  - ci
  - github-actions
  - go-test-timeout
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# A race lane names the packages that hold the shared state, and its job limit outlasts every step's test timeout

Extends docs/solutions/testing/a-race-lane-counts-only-once-it-covers-the-new-goroutines-a-canary-fails-it-and-a-race-injected-into-the-covered-code-fails-it-INFRA-271.md.

That note says to add the packages that gain goroutines. Three points it does not make:

- **Name the packages where the change's concurrency lives**: the package that holds the shared
  state and its lock, the package whose watchers or goroutines write it, and the package whose
  tests drive both at once. A change can add no goroutine to a package and still add shared state
  to it.
- **A test that writes a lock-guarded field through `reflect` or `unsafe` skips the lock.** The
  race detector reports it, but only in a lane that runs that test's package, and only on some
  runs. Change the test so it does not write the field, or write it through a method that takes
  the lock.
- **Set the job's `timeout-minutes` above the sum of its steps' worst cases**: setup, then for each
  race step its compile and its `go test -timeout`. If the runner cancels the job first, Go never
  prints the dump that names the hung test. State the sum in a comment beside the limit.

## Evidence

- INFRA-141 added the role claim view (`holders`, `holderRevisions`, `holderEpoch` under
  `holdersMu` in `internal/store/kv.go`) and the watcher completeness that feeds it
  (`kvwatch.Watcher.Complete`, `WithRescan`). CI's `envoy-go-race` job
  (`.github/workflows/envoy-and-contracts.yaml`) named only the Dispatch packages and
  `./internal/bus/`.
- The tester's `-race` run of `./cmd/listener` at a9a44ddd failed in 2 of 30 runs:
  `timeOutRoleClaimScan` (`cmd/listener/roles_from_claims_test.go`) replaced `Registry.holders`
  through `reflect.NewAt(...).Set` without `holdersMu`, while the stopped watcher still applied
  under it. 1e7885a9 removed that write (0 races in 30 runs, and 0 in a full `-race` run of the
  package) and added `./cmd/listener/` to the job. Review 5660728893 then asked for the packages
  that hold the state too; 81c75d73 added `./internal/store/ ./internal/kvwatch/...`.
- The job's limit was 45 minutes. Its first race step took 880 s on main's last green run and its
  own `-timeout` is 30 minutes, so the new step's `-timeout 30m` could never fire. 81c75d73 set
  `timeout-minutes: 75` for a worst case of 66: about 1 minute of setup, 30 for the first step, up
  to 5 for the second step's race compile, and 30 for the second step.
