---
title: "A Job That Restores the Go Build Cache Passes -count=1 to Every go test"
category: github
tags:
  - github-actions
  - caching
  - go
  - ci-router
date: 2026-11-04
status: active
module: .github/workflows
related_issues:
  - "cybermonklabs/legion#2741"
---

# A job that restores the Go build cache passes `-count=1` to every `go test`

Extends docs/solutions/github/join-a-job-to-the-ci-router.md.

- A CI job that restores `~/.cache/go-build` passes `-count=1` to every `go test` it runs.
  `GOCACHE` holds Go's test results, not only its compiled packages, so a restored cache answers
  a test whose inputs did not change and the job reports `ok … (cached)` having run nothing.
- Put the flag in the same change as the restore. The restore is what makes it necessary, and a
  job that gains a cache a week before it gains the flag reports green over tests that never ran.
- A commit changes one package and leaves every other package's test binary byte-identical, so
  the loss is almost the whole suite, not a corner of it. That is the same green a passing run
  gives, which is why no check catches it.
- The flag belongs in the step's own command, before the package paths. A pinned `step_run` in
  `.github/scripts/ci_job_reads.py` is matched as a substring of the step's line
  (`ci_filter_coverage.py:637`, `squash(r.text) in squash(line)`), and `reduce_line` then compares
  only packages, tags, mode and `-race`. So the gate fails loudly on a flag inserted before the
  pinned text and says nothing about one appended after it: insert, run
  `.github/scripts/check-ci-filters.sh`, and move the pin when it fails.

## Evidence

sjawhar/legion#2741 (dispatch://INFRA-1106) gave `envoy-go` a Go cache family of its own,
`go-envoy-test`, restoring `~/go/pkg/mod` and `~/.cache/go-build` (`ci.yaml:2549-2557`) and saving
it on a push to main (`ci.yaml:2655-2661`). The same commit put `-count=1` on the three calls that
lacked it (`ci.yaml:2596`, `:2597`, `:2604`); the integration call already had it (`ci.yaml:2632`).

Measured in the issue's pod on go1.27.1 with `GOCACHE` and `GOMODCACHE` warm, which is the state
the restore creates: `go test -timeout 20m ./internal/broker/config` printed
`ok github.com/sjawhar/envoy/internal/broker/config 0.776s`, and the second call on that same
cache printed `ok … (cached)` — the tests did not execute. With `-count=1` on the same cache, the
two calls printed `0.636s` and `0.470s`, both executed.

The gate's asymmetry was measured the same day: with `-count=1` inserted before `-tags` and before
`-timeout` and the pins in `ci_job_reads.py` left alone, `.github/scripts/check-ci-filters.sh`
exited 1 with `24 CI filter problem(s)`, naming the natslive Run and the unit step's pinned script;
after the pins moved it exited 0 over 28 gates.

`envoy-go` carried no cache restore before that change, so its test cache was always cold and the
flag's absence cost it nothing. The pairing is what to carry away: the restore and the flag are one
change.
