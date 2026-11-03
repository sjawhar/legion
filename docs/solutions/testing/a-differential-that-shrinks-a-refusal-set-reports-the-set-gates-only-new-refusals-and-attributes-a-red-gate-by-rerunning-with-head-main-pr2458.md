---
title: "A differential that shrinks a refusal set reports the set, gates only new refusals, and attributes a red gate by rerunning with head = main"
category: testing
tags:
  - differential
  - corpus
  - refusals
  - attribution
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A differential that shrinks a refusal set reports the set, gates only new refusals, and attributes a red gate by rerunning with head = main

Extends docs/solutions/testing/a-committed-corpus-differential-compiles-at-base-dumps-deterministically-and-each-surface-gets-a-negative-control-its-input-can-trigger-pr2451.md.

- When the change's goal is to store what was refused, count the refusals as a reported line, not
  a gate: some refusals are on purpose, and a corpus holding one would fail every later run. Gate
  only the documents the base stored and the head refuses.
- Print the summary and run the failing check from one list of gates, so no gate is printed and
  not enforced.
- For a character-level rule, add a corpus that enumerates every string up to a length over the
  rule's own few characters, beside the random corpora. State the arguments and the document count
  in the generator's header.
- When a gate is red against a base older than your merge base, measure whose it is. Rerun the
  flagged documents with head = current main, then with base = current main and head = yours.
  Equal counts in the first run and 0 in the second put every flag on main.
- After a forward merge, compare dumps for byte identity against the merged head. A base before
  the merge differs at every corpus document main added; count them, do not quote one.

## Evidence

- `differential_test.go` in sjawhar/legion#2458: `writerRefuses` (line 425) feeds the reported
  "refused by the writer" line and the gate "newly refused by the writer"; the `gates` slice (line
  440) drives both the summary and the `t.Errorf` loop. Refused by the writer went from 304 at the
  red-test commit e8804562 to 0 at the head.
- `gen/exhaustive-corpus.ts` (`9 '~' '\' '*' 'a'`: 349,524 documents) holds the filed defect
  `p \~\a*~* q` and every shape of its cause: 172 of the 304 refusals.
- The final run against 452e4647 showed churn 1655, lost fixed points 82 and misreads 5. The same
  documents with head = main b93381d3 gave exactly those counts, and base b93381d3 against head
  b1f5946c gave 0 on every gate.
- Round 3's `cmp` against the pre-merge 7511136b differed at 28 documents, every one a corpus file
  main's sjawhar/legion#2481 added; against the merged head 2e357b5b it exited 0.
