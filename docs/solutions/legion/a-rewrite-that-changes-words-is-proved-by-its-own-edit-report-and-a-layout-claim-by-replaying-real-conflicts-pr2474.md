---
title: "A rewrite that changes words is proved by its own edit report, and a layout claim by replaying real conflicts"
category: legion
tags:
  - regeneration
  - proof
  - merge-replay
  - git-merge-tree
date: 2026-10-31
status: active
module: .legion
related_issues:
  - "sjawhar/legion#2474"
---
# A rewrite that changes words is proved by its own edit report, and a layout claim by replaying real conflicts

Extends docs/solutions/legion/a-mass-re-layout-of-a-file-many-pull-requests-edit-is-a-script-and-each-merge-of-main-reruns-it-pr2422.md.
That file's guards assume a whitespace-only change; these prove a rewrite that also changes words.

- Make the regeneration script write a report of every edit it made (`id`, `kind`, `file`, `old`, `new`).
  Prove "nothing lost" by applying the report to the whitespace-folded base and showing zero word-level hunks left against the head, and separately that every base word appears, in order, in the head's rendered text.
  Give each check a negative control: one word deleted, one code span removed.
- Prove "concurrent edits now merge" by porting real conflicting merges onto the new layout and merging them again with `git merge-tree --write-tree`.
  Compute each pair's expected result from the two sides' hunks before the run, and keep controls that must still conflict, so a pass is not empty.
- Observed in that replay, not a git guarantee: an inserted item's blank line went to its end, so an item added directly after an edited item merged clean, and one added directly before an edited line conflicted.

## Evidence

- The issue's `restructure.ts --report`, `check-differences.ts` and `check-rendered.ts` at 119c52c7 (retro removed `.legion/`): 79, 275 and 39 report entries applied with 0 unexplained hunks; 0 missing words over 16,916, 80,696 and 90,972 base words.
- The issue's `replay/run.sh` and `replay/README.md` at 119c52c7: 36 PASS; 20 real pairs clean, 10 controls conflict.
  Pairs `990ebd7341e0` and `22817ba1d2a3` (insert after) merged clean; `b1d8a21b1e8f` (insert before the edited command line) conflicted, as classified.
- Related: docs/solutions/github/price-a-fail-wide-rule-on-replayed-heads-and-pin-the-steps-it-widens-instead-of-widening-their-filters-pr2316.md replays history to price a rule.
