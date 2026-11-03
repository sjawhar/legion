---
title: "Judge 'no new resident memory' by in-use totals and a state census, not by heap site names"
category: testing
tags:
  - pprof
  - heap-profile
  - acceptance-criteria
  - load-test
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2427"
---

# Judge 'no new resident memory' by in-use totals and a state census, not by heap site names

- Do not write a memory acceptance check as "no in-use site at head that base lacks". A heap
  profile at the end of a load window holds the working set of every request still running. A
  function the change renames or adds on that request's path shows as a site at head alone, while
  nothing is kept.
- Write it as two checks. (1) Total in-use bytes, and in-use bytes under the package, are not
  higher at head across the runs (`go tool pprof -top -sample_index=inuse_space [-focus=<pkg>]`).
  (2) A census of the change finds no new package-level variable, cache or other long-lived
  holder.
- Match each site that appears only at head to the base site it replaces, by caller
  (`-top -cum -focus=<site>`), and name both in the evidence.

## Evidence

- The plan's criterion was site-based. At head, `slices.Clone[[]pmdoc.Mark]`, `readsBackUnder`,
  `childrenAsWritten` and `writtenTree` appeared only at head. They replace base's `make` in
  `visibleMarks`, `runReadsBack`, `StripAnchorMarks` and `writtenLineBreaks`, which appeared in
  every base run (PR body, "Memory").
- Total in-use was not higher: replay base 968/1055/1103 MB, head 1026/1004/1009 MB; load test
  base 1179/1098/1083 MB, head 1113/1070/1015 MB. The only new package-level state is a parser
  context key.
- In one round, in-use under the package read higher at head on all three replay runs. The bytes
  were in `pmdoc.len16` under `refreshAnchors → FindMark → nodeSize`: the write in flight at the
  snapshot (39–267 MB across runs on both sides), in code the change did not touch.
