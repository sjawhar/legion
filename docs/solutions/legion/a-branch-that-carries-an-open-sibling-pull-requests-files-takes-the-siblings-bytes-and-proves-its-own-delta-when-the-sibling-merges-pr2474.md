---
title: "A branch that carries an open sibling pull request's files takes the sibling's bytes, and proves its own delta when the sibling merges"
category: legion
tags:
  - stacked-work
  - forward-merge
  - conflicts
date: 2026-10-31
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2474"
---
# A branch that carries an open sibling pull request's files takes the sibling's bytes, and proves its own delta when the sibling merges

- While the sibling is open, every main edit to its files conflicts your branch too.
  Never resolve those files by hand: take the sibling's own merge, or run the sibling's own script, and `cmp` the result against the sibling's head.
- When the sibling merges, restore every one of its paths from `main@origin` in one forward merge.
  First `cmp` main's copy against the copy you carried; then re-apply only your named changes, and show the diff of those files against main equals the diff the reviewer approved.
- Before you push that merge, list `jj diff --from main@origin --name-only` outside `.legion/`: it names only your own files.

## Evidence

- sjawhar/legion#2474 carried sjawhar/legion#2422's checker and its two Envoy guides.
  Merges bb0690b9, fd2a2c44 and 746077c0 existed only for main's edits to those guides; each took #2422's bytes (`cmp` against #2422's merge 1f92d90f, then cd9ecf51).
- After #2422 merged as 8a72cf79, merge 119c52c7 restored #2422's paths from main; main's checker equalled the carried copy, so the three checker files equal the approved 9c8ebad4 and the diff against main is six files.
  The name list caught a changed `bun.lock` in the merge before the push (the workspace had run `bun install`), and it was restored from main.
- Related: docs/solutions/legion/when-main-conflicts-on-the-file-faster-than-the-required-checks-finish-report-both-rates-to-the-architect-pr2422.md and docs/solutions/legion/when-a-forward-merge-brings-mains-own-copy-of-a-behaviour-the-branch-built-keep-one-and-check-the-spec-against-it-pr2376.md.
