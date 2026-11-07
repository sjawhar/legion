---
title: "A test that starts several processes builds the binary once, before any window the product times"
category: testing
tags:
  - multiprocess
  - flaky-tests
  - cmdtest
  - timeouts
date: 2026-11-04
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "sjawhar/legion#2461"
---

# A test that starts several processes builds the binary once, before any window the product times

Extends docs/solutions/testing/a-container-build-inside-go-test-spends-the-tests-deadline.md.

- When a Go test starts several real processes, build the binary once (`cmdtest.Build`) before the first start and pass it to every start (`dispatchOptions.Binary` in `packages/envoy/cmd/dispatch`). A start that builds its own binary runs `go build` again, and that build can land inside a window the product itself bounds: an admission wait, a lease, a heartbeat.
- That failure is not a timeout panic. The product acts at the end of its own window (it gives up, exits, releases what it held), and the test then asserts on a state the scenario never meant to reach. Look for it whenever a multiprocess test fails only under load.
- Before asserting on the state a timed window produces, assert by name that the process is still inside it, so a run that outlived the window fails with that reason instead of a wrong-state message.

## Evidence

- `TestAProcessStoppedWhileWaitingForAdmissionReleasesItsDurables` (`packages/envoy/cmd/dispatch/multiprocess_test.go`) started three processes, each building envoy-dispatch. The third build ran inside the refused process's 20 s admission wait (`processes.LiveWindow + processes.HeartbeatInterval`). Idle, the row used 15.0-15.6 s of the 20 s. Under three concurrent runs the build alone took 32 s, the refused process gave up at 20 s, exited 1 and released both durables, and the standby bound them instead of standing by: 0 of 36 runs passed, each failing `no line holding [msg="dispatch delivery: NATS consumer held by another process; standing by" ...] within 10s`. Serially it passed 40 of 40.
- Fix, commit 36cf0822: one `cmdtest.Build` before the first start, passed to all three, and a `select` on `waiting.Exited` that fails naming the 20 s budget. After: 50 of 50 serially (150 of 150 with two sibling runs) and 21 of 21 under three concurrent runs. The same file already used the shared-binary form in `TestAStagedKeyRotationAcrossThreeAppliesKeepsPeopleSignedIn`.
