---
title: "An adjacent-lines conflict in a one-line-per-entry file is composed from both parents and verified by a diff against each"
category: legion
tags:
  - conflict-resolution
  - forward-merge
  - AGENTS.md
  - jj
date: 2026-10-09
status: active
module: AGENTS.md
applies_when:
  - jj reports a conflict in a file whose entries are single long lines (AGENTS.md's Commands block, a table, a lock file) and the two sides edited neighbouring lines rather than the same line
  - You would otherwise retype a line of several hundred characters out of a conflict hunk
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# An adjacent-lines conflict in a one-line-per-entry file is composed from both parents and verified by a diff against each

Extends docs/solutions/legion/a-stopgap-that-lands-on-main-mid-plan-is-superseded-not-merged.md.

- When `main` and the branch each rewrote different, neighbouring lines of a one-line-per-entry
  block, jj's one conflict is two legitimate edits that happen to touch one hunk; neither side's
  version is right and hand-merging the markers means retyping lines too long to copy by eye.
  Compose instead: take the parent with more edits whole (`jj -R "$LEGION_WORKSPACE" file show -r
  main@origin <file>`), find the other parent's line by its stable prefix (`jj … file show -r
  legion/<KEY> <file>`, the line starting `legion worker-shim --connect `), assert exactly one match
  on each side, swap it in byte for byte, and write the file over the conflicted one.
- Then prove it against both parents, which replaces that note's "grep the resolved file for the
  other side's key terms": `jj -R "$LEGION_WORKSPACE" diff --from main@origin --to @ --git <file>`
  must show only your side's lines, and `jj … diff --from legion/<KEY> --to @ --git <file>` only
  `main`'s. A line in either diff that neither side wrote is a lost or doubled edit. The
  unchanged-diff fingerprint (`skill://legion-worker/references/conflicts-and-rewrites.md`) is the
  second witness: equal before and after the merge, since the branch's own added and removed lines
  did not change.
- Count both sides' edits to the file first (`jj diff --from <fork point> --to <each parent> --stat
  <file>`): the parent with the larger count is the base to compose on, and the counts say how many
  lines each verification diff may show.

## Evidence

sjawhar/legion#1848 (LEGION-629), merge 66c26bbc of `main@origin` 5c956b07, 2026-10-09. The one
conflict was AGENTS.md's Commands block: `main`'s #1857 rewrote the `legion handoff write|read` and
`legion handoff complete --summary` lines (and one table row elsewhere in the file: 3 insertions, 3
deletions against the fork point f1dcadfe), the branch rewrote the next line, `legion worker-shim
--connect …` (1 insertion, 1 deletion), each line between 1,500 and 3,000 characters. jj showed the
hunk as the branch's three lines on one side and `main`'s diff on the other. Composed as `main`'s
file with the branch's worker-shim line (index 65 in both parents), the verification read: against
`main`, one `-`/`+` pair, the worker-shim line; against the branch, three pairs, #1857's two command
lines and its ledger row; no marker left. The fingerprint `ce20b82d…` was equal at afcdd171 and at
66c26bbc.
