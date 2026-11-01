---
title: "A name your branch gave a new meaning is a seam: every merge of main brings prose in the old meaning through it"
category: legion
tags:
  - forward-merge
  - rename
  - stale-prose
  - conflict-resolution
date: 2026-11-01
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2496"
---

# A name your branch gave a new meaning is a seam: every merge of main brings prose in the old meaning through it

Extends docs/solutions/best-practices/a-symbol-citation-rots-when-the-behaviour-moves-under-the-name-pr2580.md.

- Your branch may rename a function main still calls by its old name, and give that old name to a new function. If so, every later merge of main must carry main's changes to the renamed function into it: keep your name, with main's new body, signature and comment.
- Then grep the merged head for the old name in comments, `AGENTS.md` files and READMEs, and read each hit. Main's new prose uses the old name and merges with no conflict. On your branch that name still resolves, to the other function, so build, vet, tests and a grep for dangling names all pass while the prose names the wrong code.
- List each hit you repoint in the merge comment, beside the conflicts you resolved.

## Evidence

- #2496 gave `readFork` a new meaning, the transaction's one read of the room (`readFork(ctx, write)`, `livewrite.go:440`), and renamed the fork's tree reader to `readForkTree` (`livewrite.go:729`). The rename was first resolved at merge 52f3a8f4.
- Main's #2522 then rewrote the tree reader and cited it as `readFork` in new comments. Merge 67ac05a4 resolved the five code conflicts as above. The reviewer's commit 248b5563 then had to repoint the prose the merge carried in: `tree.go`, `edits.go` and `packages/envoy/AGENTS.md` named `readFork` where they meant `readForkTree`.
