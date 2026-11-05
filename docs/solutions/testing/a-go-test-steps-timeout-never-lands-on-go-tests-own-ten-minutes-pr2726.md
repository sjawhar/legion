---
title: "A go test step's -timeout never lands on go test's own ten minutes"
category: testing
tags:
  - go-test
  - go-test-timeout
  - ci
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# A go test step's `-timeout` never lands on `go test`'s own ten minutes

Extends docs/solutions/testing/a-go-test-step-is-protected-only-by-the-timeout-it-names-itself-pr2674.md.

That note says a `go test` step is protected only by the `-timeout` it names itself, and tells you
to find which step a panic came from by the deadline it prints.
`-timeout 10m` is the hole in its own diagnostic.

- **When your sizing formula lands on 10 minutes, take the next value up.**
  `panic: test timed out after 10m0s` is byte for byte what a step with no flag at all prints, so
  a 10-minute flag destroys the one cheap way to tell a sized step from an unsized one in a log.
- **It also buys no margin.**
  A formula that produces exactly 600 s has placed the deadline on the same number whose firing
  was the defect you are fixing, with nothing between the measurement and the deadline.
- **The asymmetry decides it.**
  Five more minutes cost five minutes of a job limit that only a hang ever reaches.
  A false timeout costs a whole round: an implement, a test and a review.
  So a floor is a floor, and taking the next multiple up is a choice the sizing rule leaves you,
  not a departure from it. State the reason beside the number.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) gave the `test` job's plain step an explicit
`-timeout` for the first time.

- The acceptance criterion asked for at least 2 × the largest measured time of the slowest package
  the step tests, and the plan's rule was "the smallest whole multiple of five minutes at or above
  2 ×".
  After the change the slowest package the plain step tests is `internal/workflow`, whose maximum
  over the 30 counted jobs is 294.782 s, so the rule produces exactly 10 minutes, 1.8 % above the
  floor.
- The step carries `-timeout 15m` (`.github/workflows/ci.yaml`), 3.05 × that maximum, and the pull
  request body states the reason beside it as the one deliberate departure from the plan's
  arithmetic.
- The diagnostic it protects is the predecessor note's own: at this head a panic reading `15m0s`
  is the plain step and one reading `30m0s` is the race step, which is only true while neither
  number is 10.
- What a 10-minute deadline looks like when it fires is in the same window: job 125691048644 of
  run 41562777668 ended `panic: test timed out` in a race step that named no deadline at all.
