---
title: "A load-test CPU fix needs a seed that reaches the changed code, a load that completes it, and the function's profile share"
category: testing
tags:
  - load-test
  - pprof
  - cpu
  - merge-rule
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2425"
---

# A load-test CPU fix needs a seed that reaches the changed code, a load that completes it, and the function's profile share

Before you compare base and head runs of the Dispatch load test, `packages/envoy/loadtest`, prove
that the runs execute the code you changed. Then judge the result by that code's share of the
profile. On 2026-10-28 that package is only in sjawhar/legion#2369, which is not merged: export it
from that pull request's head into each build's source tree (`git archive <head>
packages/envoy/loadtest`) and name the commit you used.

1. **The seed must reach the changed path.** Read the handler and find each early return that an
   empty input takes, such as an empty reference closure or a list with no rows. Confirm that the
   seed fills that input. When it does not, add production-shaped data through Dispatch's own API
   before any measured run. Check the data's shape against a measured production sample on the
   property the fix depends on: for a memo, the share of repeated values; for a loop, the row
   counts.
2. **The load must complete the changed path.** A step can pass the calibration tests (box busy,
   `req/s sent`) while Dispatch waits for its connection pool. Then every request to the route
   times out and the changed function shows almost no profile time. Before the measured runs (three
   at base and three at head, alternating, each from its own reset), read the
   route's 2xx count in `summary.json`. Also read the changed function's seconds in a calibration
   `cpu.pprof` (`go tool pprof -top -show_from=<function> <file>`). If the route fails, use the next
   lower step for all runs and tell the architect.
3. **Judge by the profile share when the total CPU does not separate.** Most single fixes save
   less CPU than the spread of total cores between runs of the same build. Measure the changed
   work with `-show_from=<function> -focus=<hot callee>`, giving `go tool pprof` the three base
   `cpu.pprof` files together and then the three head files together, so each side's runs are
   summed into one profile. Divide by each side's total samples.
4. **Measure memory under the function, not for the whole process.** Use
   `-sample_index=alloc_space` and `-sample_index=inuse_space` of each run's `heap.pprof`, under
   the same `-show_from`. The heap-in-use p50 of a run has a few samples only and moves by hundreds
   of MiB between runs of one build.
5. **Prove that answers are unchanged by the body sha256, not by the ETag.** Fetch every seeded
   issue on each build after a fresh reset. Record status, ETag, body sha256 and size, then `diff`
   the two files. Use as the negative control the most plausible wrong implementation (a memo on
   the wrong key). An ETag that does not hash the changed fields stays equal on that broken build.
6. **Attach throwaway inputs to the issue.** A workspace that is recreated loses `$TMPDIR` too.
   Attach the seed program and the result bundle with `dispatch artifact`. Read a file back with
   `GET /api/v1/issues/<KEY>/artifacts/<slug>/versions/<n>`: the route without `/versions/<n>`
   answers the artifact's JSON metadata, not its bytes.

## Evidence

- The load test's seed (sjawhar/legion#2369) writes no `dispatch://` reference. So every
  references closure was empty, and `loadArtifactsByRefKey` returned before its query
  (`packages/envoy/internal/dispatch/api/references.go`, the `len(refKeys) == 0` return). sjawhar/legion#2425
  added 1,000 cited documents (19,471 versions) and 51,071 citations with a throwaway `refseed`
  program, attached to the pull request's Dispatch issue. Its shape check found a distinct
  author-list share of 5.4%, against production's 6.2%.
- At the 1x step (`--events 4 --sessions 400 --requests 60`), the calibration passed (58% busy,
  59.7 of 60 req/s sent). But 1,703 goroutines waited for a pool connection, all 741 references
  requests failed at the 30 s client timeout, and the decode ran 0.01 s of 46.86 s. At 0.5x, 413
  of 417 references requests succeeded and the decode ran 1.44 s of 69.42 s.
- At the pull request's head after main was merged into it, the Dispatch cores were base 0.441/0.489/0.472 and head 0.462/0.457/0.464:
  a spread of 0.047 cores against an expected gain of 0.0096. The decode share fell from 2.62% to
  0.16%.
- Round 1 heap-in-use p50 was base 607/1,044/953 MiB and head 1,202/1,050/1,103 MiB. In the same
  runs, the decode's allocations under the loader fell from 0.31–0.34 GB to 0.02–0.03 GB per run.
- A head whose author memo was keyed on the document returned the same 1,000 ETags, but 291 of
  the 310 bodies with references differed.
- The workspace recreation between rounds removed `/legion/scratch/tmp/infra650`. Round 2 rebuilt
  the stack from the attached `refseed` tgz, whose sha256 matched the one recorded in round 1.
