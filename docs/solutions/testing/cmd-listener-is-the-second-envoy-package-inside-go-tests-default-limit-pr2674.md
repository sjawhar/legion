---
title: "cmd/listener is the second envoy package inside go test's default limit: give it -timeout too"
category: testing
tags:
  - go-test
  - timeout
  - envoy-go
  - cmd-listener
date: 2026-11-03
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2674"
---

# cmd/listener is the second envoy package inside go test's default limit

Extends docs/solutions/testing/an-envoy-go-timeout-at-10m0s-in-dispatch-api-is-the-whole-package-near-go-tests-default-limit-on-ci-too-not-the-test-it-names-INFRA-391.md.

That document names `internal/dispatch/api` as the package that sits against `go test`'s default
600 s limit.
`packages/envoy/cmd/listener` is the second, and it reaches the limit from the other side: the
listener's tests start real listener processes and real NATS servers, so the package's time grows
with the number of them, not with any one test.

Pass `-timeout 25m` to every local run that includes `./cmd/listener`.
Read a `panic: test timed out after 10m0s` there the way INFRA-391 says: the package ran out of
time, the test named under `running tests:` is only the one running at that moment, and the run
prints **zero** `--- FAIL` lines.

## Evidence

sjawhar/legion#2674, measured in an issue pod across four implementing rounds at four heads:
613.8 s, 627.7 s, 652.9 s and 717.6 s for `github.com/sjawhar/envoy/cmd/listener` alone, every run
green, against the 600 s default.
The same command without `-timeout` reported
`FAIL github.com/sjawhar/envoy/cmd/listener 600.110s` with `panic: test timed out after 10m0s`,
naming a subtest that had run for 0 s, and no `--- FAIL` line anywhere in its output.
`-p 1` did not help; raising the limit did, at the same head and with the same code.
