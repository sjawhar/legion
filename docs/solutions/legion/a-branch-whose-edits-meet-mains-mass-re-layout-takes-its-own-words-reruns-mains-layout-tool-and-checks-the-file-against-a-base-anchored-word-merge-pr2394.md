---
title: "A branch whose edits meet main's mass re-layout takes its own words, reruns main's layout tool, and checks the file against a base-anchored word merge"
category: legion
tags:
  - forward-merge
  - conflict
  - semantic-line-breaks
  - guides
date: 2026-10-31
status: active
module: scripts
---

# A branch whose edits meet main's mass re-layout takes its own words, reruns main's layout tool, and checks the file against a base-anchored word merge

Extends docs/solutions/legion/a-mass-re-layout-of-a-file-many-pull-requests-edit-is-a-script-and-each-merge-of-main-reruns-it-pr2422.md,
from the side of a branch that edited the file before main re-laid it out.

## The rule

- For each conflicted block, first check that main changed only whitespace there: the base's text
  and main's text, with whitespace folded, are equal. When they are, your branch's text is the
  whole content of the block. Take it.
- Then run main's layout tool over the file (`bun scripts/semantic-line-breaks.ts --write <file>`),
  rather than reflowing by hand, and confirm its check exits 0.
- Verify the file against a three-way word merge of the base, your branch and main, run with
  whitespace ignored and anchored on the base's word order. The merged file, folded, must equal
  that merge exactly, with no overlaps. The base anchors every word's position, so this check also
  catches a sentence moved out of order, which a diff against each parent alone cannot see
  (docs/solutions/best-practices/a-word-level-merge-of-prose-is-verified-by-reading-the-paragraph-INFRA-42.md).
- A block where main also changed words is a real conflict: merge it by hand and read it as prose.

## Evidence

- Merge `340fc02b` of main `d7e1daaa` (#2474, one sentence per line) into sjawhar/legion#2394
  conflicted in `packages/envoy/AGENTS.md` (6 blocks) and `packages/envoy/cmd/dispatch/README.md`
  (5). A word-level merge resolved all but four, and in those four the folded base and main texts
  were equal.
- After this pull request's text was taken in those four and the layout tool run, a
  whitespace-insensitive three-way word merge of `e018f1d3` (base), `bfd5cf69` (branch) and
  `d7e1daaa` (main) equalled each file exactly, with 0 overlaps, and
  `semantic-line-breaks.ts` exited 0.
