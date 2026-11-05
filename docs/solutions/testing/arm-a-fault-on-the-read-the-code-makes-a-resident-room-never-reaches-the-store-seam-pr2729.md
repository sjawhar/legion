---
title: "Arm a fault on the read the code makes: a resident room never reaches the store seam"
category: testing
tags:
  - failure-injection
  - test-seams
  - flaky-test
  - read-cache
  - dispatch-documents
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2729"
---
# Arm a fault on the read the code makes: a resident room never reaches the store seam

Extends docs/solutions/testing/failing-one-query-of-a-real-server-by-shadowing-a-function-on-search-path-INFRA-184.md.

That document says where to put a failure when the server must stay unmodified. This one says how
to choose the layer: arm the fault on the call the code under test makes, not on a layer below it
that the code may skip.

- Before you arm a one-shot failure on a store, read the path from the code under test down to
  that store and look for a layer that can answer without it. In Dispatch a document read is
  served from the room when the room is resident (`heldTree`,
  `packages/envoy/internal/dispatch/docs/mutation.go`) and reaches the store only through
  `coldRead` (`docs/read_cache.go`). A test whose fixture leaves the room resident — creating an
  issue does — never takes a failure armed on the store.
- Such a test does not fail: it passes for the wrong reason whenever the bypass is taken, and
  fails only when the room happens to be gone. That is a flake whose rate depends on timing, so
  it looks like somebody else's race. Measure before you believe that: `-race -count=<n>` on the
  test alone gives the rate in minutes.
- Arm it instead on the interface the code under test calls, and say in the double's doc which
  call that is and why the lower layer would not do. In package `api` that interface is
  `docs.API`; a test server takes one with `newInteractionHandler(t, func(*store.Store) docs.API)`.
- A fault-injection test is finished only when it can still fail. Assert the thing that holds only
  if the arming was taken (`coverage_unread`, not merely a 200), so a read that skipped the
  injection reds the test rather than passing it.

## Evidence

`TestApprovalRequestGoesThroughWhenTheCoverageReadFails`
(`packages/envoy/internal/dispatch/api/coverage_test.go`) armed its one-shot failure on
`docs.VersionedStore` (`DocumentStamp`, `LoadDocument`). The coverage read it meant to break is
`requirementBlocksIn` → `typedBlocksIn` → `deps.Docs.Text` (`api/results.go:510`), and the issue's
own creation leaves the spec's room resident, so the read usually answered from the room and the
arming was taken by nothing. Measured: 3 failures in 120 runs on `main`, 7 in 90 at the pull
request's head, two red CI attempts of `envoy-go-race-api` in run 41800939982, both at
`coverage_test.go:291` ("want coverage_unread with no gap lines").

The fix replaced `unreadableSpecStore` with `unreadableSpecDocs`, a `docs.API` whose `Text` fails
once for one artifact id. `CGO_ENABLED=1 go test -race ./internal/dispatch/api -run
TestApprovalRequestGoesThroughWhenTheCoverageReadFails -count=200` is then `ok 468.206s`, and the
reviewer, the tester and both thermonuclear passes ran it green at -count=30, 30, 50 and 200.
