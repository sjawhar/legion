---
title: "Never rename another pull request's unreleased changelog fragment: git reads rename against delete as a conflict, jj does not"
category: legion
tags:
  - changelog
  - jj
  - git
  - merge
  - forward-merge
date: 2026-10-31
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2584"
---

# Never rename another pull request's unreleased changelog fragment: git reads rename against delete as a conflict, jj does not

- **Fix a naming-rule breach only in the fragment your own pull request added.** Another pull
  request's unreleased fragment is on its way to the changelog fold, which publishes its entry
  into `CHANGELOG.md` and deletes the file. Your rename then races that fold, and when it loses
  you have a rename against a delete.
- **Keeping the renamed copy at that merge republishes a published entry.** The fold has already
  written those lines into `CHANGELOG.md`; a fragment put back is folded a second time. The
  resolution is to take the base's deletion, keeping only the fragment your own pull request
  added.
- **A clean `jj` merge is not evidence that GitHub will merge.** `jj` does no rename detection, so
  it reads your rename as an add plus a delete and merges silently: the add is yours alone, the
  delete is both sides'. Git, which pairs renames, reports `CONFLICT (rename/delete)`. A branch
  can therefore look mergeable in the workspace while GitHub's merge conflicts — and GitHub may
  still answer `mergeable: MERGEABLE` for a while, against a stale `baseRefOid` it has not
  recomputed.
- **So read the base's own answer, not only the workspace's.** Check `baseRefOid` against the
  base's tip; where a rename of a file the base may have moved is in your diff, merge in a scratch
  clone with git before you trust either.
- **A reviewer asking for the rename owns this too.** The rule it serves is about a fragment
  waiting to be published; a fragment the base has already published and deleted is out of its
  reach.

## Evidence

sjawhar/legion#2584. Round 2's review had the branch rename three fragments belonging to #2504 and
#2553, to strip Dispatch issue keys from their names (the root `AGENTS.md` rule: no Dispatch key in
a fragment's file name, `Issue` field or body). Main's `f4ec92ee5`, `chore: fold changelog fragments
[skip ci]`, then published those entries and deleted the files, leaving one fragment in
`packages/envoy/.changes/unreleased/`.

- The tester reproduced it with a real `git merge` in a scratch clone: three
  `CONFLICT (rename/delete)` lines, one per renamed file, and nothing else.
- `jj new legion/<KEY> main@origin` in the workspace reported no conflict at all and kept all
  three renamed files, which is the double-publish state.
- GitHub answered `mergeable MERGEABLE` throughout, with `baseRefOid` four commits behind the
  base's tip.
- The resolution, approved by the tree's architect, removed all three in the merge commit, leaving
  the base's own fragment and the one this pull request added. The branch's diff then changed by
  exactly those three files leaving it: the other nineteen were compared one at a time and their
  added and removed lines were byte-identical.
