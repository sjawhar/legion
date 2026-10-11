---
title: "An unpushed forward merge is re-parented with jj rebase -r when main moves under it, and is described then left with jj new before legion push"
category: legion
tags:
  - forward-merge
  - retarget
  - jj
  - legion-push
  - conflict-resolution
date: 2026-10-11
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# An unpushed forward merge is re-parented with jj rebase -r when main moves under it, and is described then left with jj new before legion push

Extends docs/solutions/legion/a-stacked-branchs-retarget-is-two-forward-merges-the-bases-final-head-then-main-and-the-main-equals-base-audit-runs-over-the-whole-working-copy-LEGION-668.md.

- Fetch again before you push a forward merge of `main`. A merge of forty conflicted files takes
  long enough for `main` to move (here, five release commits and two features landed between the
  `jj new <branch> main@origin` and the push), and a merge whose `main` parent is stale pushes a
  head that differs from `main` in files your branch never touched — the retarget's "main equals
  base" audit would flag every one of them.
- When it has moved and the merge is still unpushed, re-parent it in place rather than redoing
  it: `jj -R "$LEGION_WORKSPACE" rebase -r @ -d legion/<KEY> -d main@origin`. jj re-merges the
  files `main`'s new commits touched against your resolution and keeps every other resolution as
  written; the twelve hand-composed files survived untouched here. This rewrites nothing pushed —
  `@` is your own unpushed commit — so it is not the rewrite the fingerprint procedure guards.
  Re-run the audit afterwards: `jj diff --from main@origin --to @ --name-only` sorted against the
  branch's own file list (`jj diff --from <fork> --to <branch tip> --name-only`) must leave only
  files the branch changed (here, exactly the renamed migration).
- A merge made with `jj new … -m` *is* the working copy: `@` carries the description and the
  resolved tree. `legion push` sets the bookmark on `@-`, so `jj describe` the final message, then
  `jj new`, then `legion push` — otherwise the push moves the bookmark to the merge's parent and
  pushes nothing of the merge.
- The merge's own commit is the one place for the contract renumber, the migration rename and
  every prose site those move (the collision note, the CHANGELOGs, `AGENTS.md`, the TS contract
  comments): one commit above the approved head, nothing of the branch's content rewritten.

## Evidence

LEGION-663 (sjawhar/legion#1874), the retarget after #1843 squash-merged. The first merge
`89fbc8a3` had `main@origin` at `4ee38a38`; by the time its checks were green, `main` was at
`9b5a6854` (pi-legion v8.13.0) and the "differs from main but not touched by the branch" list was
68 files long, all `main`'s own. `jj rebase -r @ -d legion/LEGION-663 -d main@origin` produced
`96cc9e97` with no new conflicts; the list shrank to the renamed migration alone, and the pushed
merge `cee5c6fc` passed every workflow. The same shape recurred for the second forward merge
`6da3ade3` (contract 20 → 21 after #1878), prepared while another role held the phase and pushed
after one more fetch.
