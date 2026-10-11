---
title: "A stacked branch's retarget is two forward merges, the base's final head then main, and the main-equals-base audit runs over the whole working copy, flagged files included"
category: legion
tags:
  - jj
  - forward-merge
  - stacked-pr
  - squash-merge
  - retarget
  - conflict-resolution
  - merge-audit
date: 2026-10-11
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A stacked branch's retarget is two forward merges, the base's final head then main, and the main-equals-base audit runs over the whole working copy, flagged files included

Extends docs/solutions/legion/a-stacked-branch-whose-base-squash-merged-conflicts-with-its-own-content-merge-and-fingerprint-against-the-old-base-commit-not-jjs-fork-point-LEGION-629.md.

- Retarget in two hops, not one. First `jj new <branch> <base's final SHA>`: the base's bookmark is
  gone after the squash, but its last head is still in the shared clone (the base pull request's
  `headRefOid` at merge time), and the merge base is the exact base head your branch last merged,
  so the only conflicts are the base's own later edits against your lines — seven files here. Then
  `jj new @ main@origin`: its merge base is the `main` commit the base last forward-merged, and
  every conflict is either the base's content on both sides (`M == B` in that document's terms:
  `main` does not differ from the base's final head in that file — take yours) or a sibling's
  change `main` took after the base's head (`M != B` — read and merge by hand). One hop mixes the
  two into every file; two hops let you decide the base's delta first and then classify the rest
  by one comparison.
- The `M == B` predicate is also the audit, and it runs over every file the working copy changed
  against the first hop's tip — not only the files jj left unresolved, and inside files it did flag,
  outside their conflict hunks too. A 3-way merge sees content the base added after the merge base
  and your branch then deleted as "theirs adds, yours unchanged", and re-adds it cleanly. Compute
  `files(working copy != hop-1 tip) − files(main != base's final head)`; anything left is drift to
  put back from the hop-1 tip (`jj file show -r <hop-1> <path> > <path>`). `go build` catches an
  import, nothing catches a dead method or a duplicated name list.
- jj's rendering of a two-sided conflict picks per file which side is the `+++++++` snapshot and
  which is the `%%%%%%% diff from: <base>` / `\\\\\\\ to: <side>` pair. A script that resolves "to
  mine" must find its side by the commit id in those headers, never by position: half the files of
  one merge put yours first, half put main's.

## Evidence (LEGION-668, sjawhar/legion#1878)

Stacked on `legion/LEGION-631` (#1843) at `5707759`; 631 moved to `28ea8a65` and squash-merged as
`e8fcbd2f`. Hop 1 (`b7a14c05`, base `5707759`): 7 conflicts, each 631's later vocabulary against
this branch's clauses. Hop 2 (`530e753d`, base `eb7c0e51`): 32 conflicts, 24 with
`main == 631's head` in the file (taken from this branch mechanically), 8 with sibling changes
(#1894's `DecidesRound` export, #1888's wording, three prose files, the architect prompt). The
audit over the whole working copy found `holdsGitHubCredential` back on both pod kinds, a
`gh-<role>` entry back in `legionVolumeNames` and a test message, in three files jj had flagged —
the drift sat outside their hunks — while `go build`, `go vet` and the sandbox package's tests had
all passed with it in; `go build` alone caught relaunch.go's re-added `ghconfig` import. The first
resolver script assumed one header orientation and misfired on 11 of 24 files until it matched the
`b7a14c05` id in the `+++++++` or `to:` line. The retarget comment on the pull request
(issuecomment-6105672308) carries both fingerprints and the per-file list.
