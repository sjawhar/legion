---
title: "Prove a reuse change against an independent recomputation on the live commit stream, and assert every path counter"
category: testing
tags:
  - equivalence
  - shadow
  - counters
  - negative-control
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2453"
---

# Prove a reuse change against an independent recomputation on the live commit stream, and assert every path counter

When a change replaces a computation with a reused result and keeps the old computation as the fallback:

- Compute the expected value independently, on the real event stream, at every step. In Dispatch, open a second `crdt.UpdateLog` on each room document (wrap `OnLoadDocument`). Apply its base and each logged update to a copy in commit order, and render after each one. Compare the replica's markdown, the recorded update class, the ask blocks and every version's `content_changed` with that copy.
- Count each path (reused, skipped, recomputed) with an atomic counter on the service, and have every test assert the exact change of every counter. A test that checks only the output passes when the code falls back to recomputing, so it proves nothing about the reuse.
- Use the counters to show which path a test took. Use pprof to measure what the change costs or saves. They answer different questions; one does not replace the other.
- Break each guard on purpose, one at a time, and record which tests fail and the failure line. Every guard must fail at least one test on the output itself.

## Evidence

- `packages/envoy/internal/dispatch/docs/render_reuse_test.go:25` ("never from the code under test"); `watchRenders` at `:150` opens the second log with `doc.OpenUpdateLog()` at `:162`.
- `renderPathCounts` in `tree.go:384`. The service never reads it; the tests and a throwaway counter-logging build do. That build measured the replay's mix: observer reused 34, already in the room 214, rendered 0.
- sjawhar/legion#2453's body has the four-guard negative-control table, each guard with the cases it fails.
