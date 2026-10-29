---
title: "Before reusing a ranking's pprof command, check its ignore lists against the base's stacks and focus named functions"
category: testing
tags:
  - pprof
  - measurement
  - merge-rule
  - load-test
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2453"
---

# Before reusing a ranking's pprof command, check its ignore lists against the base's stacks and focus named functions

Extends docs/solutions/testing/a-gain-smaller-than-the-run-to-run-spread-is-judged-by-the-functions-profile-share-pr2429.md.

When a plan tells you to measure a function's share "with the ranking's commands" written against an older build:

- Run `go tool pprof -peek '<focus>$'` on a base profile first, and read the callers. If an `-ignore` regex of the ranking matches a frame above the function you measure, it drops the very samples you are measuring. Leave that list out, and say so in the pull request with the reason.
- Focus named functions (`-show_from='docs\.\(\*Service\)\.applyLive$' -focus='docs\.documentMarkdown$'`), never closure names such as `applyLive.func2`. Their numbers move when the code changes, so the base and head would be measuring different code.
- Read the focused total from the cumulative column (`-top -cum`, the first row's `cum`), or with the flags in the file this extends. Before using a number, check it against `-peek`'s total for the function.
- Before the head runs, state the expected gain from path counts measured on the replay: G = base share × (paths that skip the work ÷ all calls). Record it with the base numbers where its version history shows the time, such as a Dispatch artifact.

## Evidence

- At base `59f6647db9d7`, `applyLive` runs under `MarkQuote.func1` and `applyOpsUnconditional.func1`, which the ranking's Appendix A ignore lists drop. Applied as written, they read 0.40 s for the before-render and 0.47 s for the observer render, against `-peek` totals of 8.44 s and 13.62 s.
- A first share script read the "Showing nodes accounting for" line with `-nodecount=1`. That line is the shown node's flat time, not the focus total, and it gave the same 0.40 s.
- sjawhar/legion#2453's body, "Merge rule": G = 4.06% of samples, recorded in version 1 of the measurement artifact before the first head run.
