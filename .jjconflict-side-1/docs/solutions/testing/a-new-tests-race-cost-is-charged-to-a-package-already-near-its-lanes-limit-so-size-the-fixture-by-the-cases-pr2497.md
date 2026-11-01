---
title: "A new test's race cost is charged to a package already near its lane's limit, so size the fixture by the cases"
category: testing
tags:
  - race-detector
  - ci
  - fixtures
  - test-cost
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2497"
---

# A new test's race cost is charged to a package already near its lane's limit, so size the fixture by the cases

- Before you add tests to a Go package, read that package's time in the `-race` lane on main
  (`envoy-go-race-docs` runs `go test -race -timeout 60m ./internal/dispatch/docs/`), and compare
  it with the lane's `-timeout`. `docs` took 1706–1746 s of 1800 s on main while it shared
  `envoy-go-race`'s 30-minute step, before it got that job of its own.
- Time your new tests under `-race` locally (`CGO_ENABLED=1 go test -race -run '<names>' -v`).
  Their sum is what they add to the package in that lane.
- Build a permanent test's fixture as the smallest document that still exercises each case:
  every case, mode and assertion stays, only the size goes. A document of the same shape works
  (`forkReadsDocument` in `docs/fork_reads_test.go` emits only the sections the cases quote).
- Keep the real-size proof in the measurement runs and the PR body (an HTTP equivalence run, a
  benchmark on real documents), not in `go test`.
- An overrun also drops every test that runs after the timeout, and one that refuses to start
  near the deadline (`TestThreeBrowsersAndTheAPIEditOneRoomAtOnce` needs 90 s left), so a green
  `-race` lane on a branch that adds tests to a near-limit package is the check, not the unit run.

## Evidence

sjawhar/legion#2497 added three test files on the ~400 KB `renderReuseDocument`. Under `-race`
they took 136.23 + 98.64 + 44.50 = 279.37 s, and CI run 40852446247 timed `docs` out at
1801.513 s. On a 7.3 KB fixture of the same shape, with no case or assertion removed, they took
about 8 s under `-race`, and the lane passed `docs` in 1641.5 s. The three negative controls
(the panic release reverted, `buildFork`'s `dropRendering` removed, the reuse counter removed)
still failed on the small fixture.
