---
title: "A changed message is asserted in test files the change never touched: search by behaviour, and run those tests in the pod against a local nats-server"
category: testing
tags:
  - worker-pod
  - nats
  - testnats
  - error-messages
  - stale-assertions
  - capability-claim
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-182"
  - "sjawhar/legion#2170"
---

# A changed message is asserted in test files the change never touched

Extends docs/solutions/testing/a-pod-drives-the-rendered-configuration-itself-and-a-plans-ci-only-claim-is-checked-against-docs-solutions-INFRA-127.md.

That page says a plan's "CI only" claim must be checked. This page adds what that claim costs on a
change that alters a rendered string: an error message, a log line, or a URL's rendering.

- **The blast radius is the behaviour the change alters, not the files it touches.** Before the
  push that changes a rendered string, search every package's tests, `scripts/`, `docs/` and
  `.github/` for assertions on the old rendering.
  - Also search for assertions whose expected value comes from a fixture. In `packages/envoy`,
    `testnats.URL(t)` returns `nats://tN:tN@host:port`, so a test that compares a refusal with that
    URL asserts on userinfo without spelling any.
  - Searching the message's literal text finds only half of these tests. Also search the names of
    the values the message is built from, such as `remote` and `err.Error()`.
- **Run what the search finds, and the new container-backed tests, in the pod before you push.**
  The pod has no Docker, but a real `nats-server` installs without it (the INFRA-117 page). Point
  each test at the local server instead of `testnats.URL`: use a throwaway copy, `go test -overlay`
  (the INFRA-127 page), or a git archive of the head.
  - Pushing to find out whether CI is green spends a whole review round on each test you missed.
  - The tester reproduced CI's exact failure this way in the pod.
- **When the plan says a testcontainers test runs only in CI, test that claim before you rely on
  it.** For anything `testnats` starts, it is false.

## Evidence

INFRA-182 (sjawhar/legion#2170).

- **The missed tests.** Round 1 of review made the remote-NATS refusal
  (`packages/envoy/internal/bus/reach.go:48`) name the server without its userinfo. The implementer
  searched only the files the change touched. Three tests in `internal/bus/remote_nats_test.go`,
  which this pull request never touched, compared the refusal with
  `remoteLookingURL(t, testnats.URL(t))`, a `user:password` URL. They failed in `envoy-go-race` at
  dd2fb2b41ecf and cost round 3 of review.
- **The plan's claim.** The plan recorded "the only runner of a testcontainers test is the pull
  request's own envoy-go job" (finding F7, spec departure A1), and nobody checked it.
- **Untested tests.** The new tests went to CI without ever running, and their first run was red:
  a relay that kept a joined connection alive, and a 5 s wait for a 10-19 s notice. See
  docs/solutions/envoy/nats-go-facts-a-test-or-a-shutdown-path-must-design-around-INFRA-182.md.
- **The search, done late.** In round 5, before the push, the implementer searched for assertions
  on every string the change altered and found nothing else. The tester ran the three refusal tests
  from git archives against a local nats-server v2.11.4 started with `--user t7 --pass t7`. They
  passed at the fix, failed with the redaction reverted, and, run from the old test files, failed
  with the exact message CI had reported.
