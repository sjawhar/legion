---
title: "A windowed measurement of pull requests counts those alive in the window and decides a fix by ancestry"
category: github
tags:
  - measurement
  - gh
  - merge-tree
  - jj
date: 2026-10-31
status: active
module: scripts/conflicts
related_issues:
  - "sjawhar/legion#2411"
---

# A windowed measurement of pull requests counts those alive in the window and decides a fix by ancestry

- List candidates with two reads and take their union: GitHub's search `updated:>=<window start>` (any state) and `gh pr list --state open`.
  An open pull request nothing touched since the window opened is in the second list only.
- Then count only the pull requests that existed in the window, from each record's own fields: created by the window's end and not merged or closed before its start, or carrying an event dated inside the window.
  A run made days after the window then counts the same pull requests as one made as it ended.
- Refuse a search that reaches GitHub's 1000-result cap instead of counting a truncated list.
- Decide whether a fix commit reached a merge by ancestry (`git merge-base --is-ancestor <fix> <parent>`), never by date: jj rewrites committer dates, so a date says nothing about which side of a fix a commit sits on.
  A fix that is an ancestor of the upstream parent but not of the branch parent is a transition state of its own, not a failure of the fix.
- A missing object (a force-pushed head GitHub no longer serves) is reported as undeterminable, never counted as clean.

## Evidence

`scripts/conflicts/github.ts` `listPullRequests` unions the two lists (`unionByNumber`); `scripts/conflicts/summary.ts` `existedInWindow` and `prsRead` narrow them; `scripts/conflicts/analyse.ts` `classifyFixes` gives `before`, `transition` and `after` by ancestry.
Before the open list was added, the tester re-ran one window and read 19 pull requests where the fixed code read 61.
The integration run from #2419's merge to 2026-10-31 04:00Z found 0 conflicting merges in the `after` state on every fixed file: each conflict on a fixed file came from a branch cut before the fix or still carrying edits made before it.
