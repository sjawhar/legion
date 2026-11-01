---
title: "A Go heap figure for 'no resident memory added' comes from a forced GC with no request in flight"
category: testing
tags:
  - pprof
  - heap
  - memory
  - load-test
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2453"
---

# A Go heap figure for 'no resident memory added' comes from a forced GC with no request in flight

Extends docs/solutions/testing/a-load-test-cpu-fix-needs-a-seed-that-reaches-the-changed-code-a-load-that-completes-it-and-the-functions-profile-share-pr2425.md.

When the acceptance says a change adds no memory the server keeps resident:

- Take the heap profile with `/debug/pprof/heap?gc=1`, a short time after the load ends, when no request is in flight. A profile taken at the end of a step, without the forced GC, holds the allocations of whatever request was still running.
- Compare `-sample_index=inuse_space` under the changed package (`-top -cum -focus='<module>/<package>\.'`) between the median run of each side, and measure the base-against-base spread first. A single base/head pair inside that spread measures what happened to be resident at the snapshot, not the change.
- When a number moves by more than the limit, read the `-diff_base` profile before you trust it, and compare the allocation sites the change could hold on to. In-flight request frames are timing, not resident memory.
- Prove separately that the new fields are released, with a test that asserts each one is nil after the commit.

## Evidence

- sjawhar/legion#2453: end-of-step profiles gave 706 MB at the base and 750 MB at the head (+6.2%) under `docs.*`. The `-diff_base` showed `applyLive` +151 MB and room loads −95 MB, which are requests in flight. A forced-GC pair 20 s after the step gave 409.3 MB against 396.1 MB. The tester's forced-GC pairs were +1.5%, +16% and +3.2%, against a base-against-base spread of 551-659 MB (18%). The median run of each side was 658.57 MB against 639.83 MB (−2.8%), and the render strings the change could retain were lower at the head in every pair.
- `TestRenderReuseACommittedWriteKeepsNoRendering` and `TestAColdWriteLoadsNoRoomUntilItCommits` assert that `write.renderings` and `write.forkMarkdown` are nil after the commit.
