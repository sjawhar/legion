---
title: "Name the process that wrote a log line before you build a fix on it, and measure which branch a process test can reach"
category: testing
tags:
  - logging
  - flaky-tests
  - instrumentation
  - test-helpers
date: 2026-10-30
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2542"
---

A test that captures a whole process's output attributes every line to the binary under test. When
the test file also builds helper clients and stores of its own, some of those lines are the test's.

## Rules

- **Find the writer before the cause.** The two ERROR lines that this round's premise rested on came
  from the test process's own second interest registry (`store.Open(writer)` in the other-listener
  helper of `cmd/listener/restart_test.go`), whose connection the test closes while its first
  watches are in flight: live watchers, ending at ERROR by design. Filtering a process-wide capture
  by the client name its caller passes is the mechanism
  (`docs/solutions/testing/filter-a-process-wide-log-capture-by-a-client-name-the-caller-passes-in-pr2506.md`).
- **Measure which branch a process test can reach before claiming it as a pin.** The listener binary
  opens all three caches and waits out their warm-up gates before it adds the reconnect hook
  (`cmd/listener/main.go`), so no first watch of the binary is ever in flight when a rewatch can
  replace one: the repaired branch is unreachable from any listener-process test, probed at 4.8 s
  and 40 s freezes. Its only pin is the package's own unit test
  (`internal/kvwatch/kvwatch_test.go`), and the pull request says so under `Not proven / risk`.
- **A wrong premise in the task is reported with its measurement, not worked around.** The fix still
  belonged in the change (the branch is reachable in production through the reconnect hook's
  rewatch); what changed was the claim about where it was observed.

## Coverage

Extends the log-capture page above with the attribution step that comes before filtering, and adds
the reachability measurement, which no page states.
