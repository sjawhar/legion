---
title: "A stub child the test must outlive an observation waits for a release file the test writes, never a sleep"
category: testing
tags:
  - go-test
  - flaky-tests
  - load-testing
  - stub-process
  - refresh-loop
  - controller-start
date: 2026-10-10
status: active
module: packages/daemon/cmd/legion
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A stub child the test must outlive an observation waits for a release file the test writes, never a sleep

Extends docs/solutions/testing/await-the-event-not-a-tick-budget.md.

- The Bun rule holds for a Go test whose subject is a child process: a stub (the recording
  `omp` script) that must still be running when the test observes something polls for a release
  file and exits only once it exists; the test creates the file after it has seen what it needs
  (`testwait.Eventually` on the file the loop writes, on the log line it appends). A fixed
  `time.Sleep` before the trigger and a fixed poll budget in the stub both pass alone and fail
  under the full suite's parallel load, where the trigger lands before the first write or the
  budget runs out before the loop's tick.
- Bound the stub's wait anyway (two minutes) so a test that never releases it still ends, and
  put the release file in `t.TempDir()`.

## Evidence

`TestControllerStartRefreshesTheGitHubCredentialWhileOhMyPiRuns` and
`TestControllerStartStopsTheRefreshLoopWhenTheCapabilityIsSuperseded`
(`packages/daemon/cmd/legion/controller_test.go`) first flipped the token after `time.Sleep(200ms)`
and had the stub poll five seconds; both passed with `-count=3` alone and failed when `go test
./...` ran `admit`, `api`, `workflow` and `workspace` beside them. Reworked at 930cb6ea
(`controllerOptions.waitForFile`, `waitForFileScript`), they passed under the same load.
