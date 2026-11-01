---
title: "An envoy-go timeout at 10m0s in dispatch/api is the whole package near go test's default limit, on CI too, not the test it names"
category: testing
tags:
  - ci
  - envoy-go
  - go-test
  - timeout
  - rerun
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# An envoy-go timeout at 10m0s in dispatch/api is the whole package near go test's default limit, on CI too, not the test it names

Extends docs/solutions/testing/a-loaded-devbox-stretches-every-wall-clock-budget-in-the-envoy-and-pi-envoy-suites.md.

That document lists this panic as a devbox symptom, "green on CI", and gives the package 71 s on
CI. Both no longer hold: on CI the package now takes most of the 10 minutes by itself.

- CI's `envoy-go` job runs `go list ./... | grep -v /integration | xargs go test` with no
  `-timeout` (`.github/workflows/envoy-and-contracts.yaml`, step "Unit test envoy Go package"), so
  `internal/dispatch/api` gets go test's default 10 minutes as one package.
- `panic: test timed out after 10m0s` there means the package's total time ran out. The test
  under `running tests:` is only the one that was running at that moment. Read its elapsed time:
  a few seconds means it is not the cause. Do not chase or report a hang in it.
- Re-run the failed job once (`gh run rerun <run id> --failed`), as for any red that failed on
  its own. The job is not one of main's required checks.

## Evidence

- sjawhar/legion#2209 at retro head `e5b7a07d8e90`: Legion Envoy and Contracts run 40272340448,
  attempt 1, failed with `panic: test timed out after 10m0s` and
  `FAIL github.com/sjawhar/envoy/internal/dispatch/api 600.073s`. The test under
  `running tests:` was `TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled`, which had run for
  4 s. Attempt 2 at the same head (job 121434624525) printed
  `ok github.com/sjawhar/envoy/internal/dispatch/api 531.564s` and
  `ok github.com/sjawhar/envoy/internal/dispatch/docs 379.098s`.
- The retro's fresh-eyes reader sampled 13 green `envoy-go` jobs on 2026-10-26, between 03:45Z
  and 05:27Z. The `internal/dispatch/api` line ran from 395.6 s to 596.1 s, median 511.4 s. The
  slowest was job 121399718352 (run 40268683149), 4 s under the limit. It also found the same
  panic in dispatch://INFRA-49/comment/f8f69c7f-58c1-4af7-ac6c-39e811467044 (run 40205972235)
  with another test named, `TestTwoSweepRunnersRunThePassOnceAndSkipOnce`, at 0 s.
