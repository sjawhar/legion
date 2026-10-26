---
title: "A race lane counts only once it covers the new goroutines, a canary fails it, and a race injected into the covered code fails it"
category: testing
tags:
  - race-detector
  - go-test
  - cgo
  - zig
  - worker-pod
  - positive-control
  - mutation
date: 2026-10-25
status: superseded in part by INFRA-257 — the worker image now carries gcc and libc6-dev with cgo off by default, so a role runs `CGO_ENABLED=1 go test -race …` with no `CC`, zig or linker override, and the zig wrapper applies only to an older pinned image digest without gcc; the three steps stand
module: packages/daemon
related_issues:
  - "INFRA-271"
  - "sjawhar/legion#2179"
---

# A race lane counts only once it covers the new goroutines, a canary fails it, and a race injected into the covered code fails it

Extends docs/solutions/testing/a-held-clock-test-of-timers-armed-from-postgres-times-holds-now-at-microseconds-and-a-first-race-run-indicts-old-log-sinks-LEGION-694.md.

A clean `-race` run counts as race coverage only after the three steps below, taken in order. Each
step is what makes the next one mean something. If you skip any of them, a clean run reads as
coverage when it is none.

1. **Put the packages that gain goroutines into the race lane.** CI's step
   (`.github/workflows/pr-and-main.yaml`, "Test the concurrent packages under the race detector")
   runs only the packages it names. A clean result there says nothing about a package it does not
   name. When a change adds goroutines to a package, add that package to the step in the same
   pull request.
2. **Show the detector can fail.** Under the same toolchain, run a canary: two goroutines
   increment one `int`. It must print `WARNING: DATA RACE` and fail. A clean run from a detector
   you have not seen fail is not evidence.
3. **Show the lane catches a race in the covered code.** Remove one lock in the new code
   temporarily, run the lane, see it fail, and revert. That proves the lane's tests exercise the
   shared state, not just that the packages compile with `-race`.

The LEGION-694 note says "A worker pod has no cgo, so `-race` runs nowhere but CI". That is no
longer true. Run `uv pip install ziglang` into a venv under `$TMPDIR`. Write a wrapper,
`exec <venv>/lib/python3.12/site-packages/ziglang/zig cc -target x86_64-linux-gnu "$@"`, and a
`c++` twin. Then run `CGO_ENABLED=1 CC=<wrapper> go test -race …` from `packages/daemon`. Since
INFRA-257 the image carries gcc: drop the wrapper and `CC` except on an older pinned image digest.

## Evidence

- INFRA-271 added the session proxy's accept loop, copy pairs, `track`/`untrack` and the bounded
  drain in `Close` (`internal/sessionproxy`), plus the launcher's wiring of it. CI's race step
  covered only `./internal/daemon/ ./internal/record/`. Two agents in the tree had reported "no
  gcc, `-race` not run". The tester installed zig and ran the new packages under `-race`: 47 PASS,
  no race. That was one run, not a lane.
- A retro send-back added `./internal/sessionproxy/ ./internal/launcher/` to the CI step
  (`34b2b7a999e4`). The job's env sets `LEGION_TEST_PG_DSN`, so the database tests run under the
  detector instead of skipping.
- At that head the tester's canary printed `WARNING: DATA RACE` and failed. With the lock removed
  from `Proxy.untrack`, `go test -race ./internal/sessionproxy/` reported four `DATA RACE`s in
  `(*Proxy).untrack`. It failed four tests: TestProxyCarriesBytesBothWays,
  TestEachConnectionDialsItsOwnUpstream, TestCloseEndsTheProxy and
  TestCancellingTheContextLeavesAStartedProxyServingUntilClose. The lock was then reverted.
  CI's Tests run 40185462879, with the new step, passed.
