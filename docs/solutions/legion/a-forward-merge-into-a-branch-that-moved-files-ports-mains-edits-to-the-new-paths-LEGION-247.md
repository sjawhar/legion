---
title: "A forward merge into a branch that moved files ports main's edits to the new paths, re-takes main's serials, and re-runs the proof at the merged head"
category: legion
tags:
  - jj
  - forward-merge
  - conflict-resolution
  - file-moves
  - package-split
  - daemon-api-contract
  - fingerprint
date: 2026-10-07
status: active
module: packages/pi-legion
applies_when:
  - GitHub reports a branch CONFLICTING and the branch has moved or renamed files main has since edited
  - A conflict shows as "2-sided conflict including 1 deletion" at a path the branch deleted
  - Main bumped a version, contract number or lockfile the branch's manifests copy
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A forward merge into a branch that moved files ports main's edits to the new paths

Extends docs/solutions/legion/long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md.

- A conflict `including 1 deletion` at a path your branch moved is main's edit to the old path.
  Read main's hunks for that file (`jj diff --from <last base> --to main@origin --git <old path>`),
  apply them by hand to the file's new path, then accept the deletion with
  `jj resolve --tool :ours <old path>`. A three-way merge of base, your new-path copy and main's
  old-path copy on scratch files does the bulk of a large file.
- A file main *added* under a directory you relocated arrives at the old path with no conflict
  marker at all. After resolving, list main's additions in every directory you moved
  (`jj diff --from <last base> --to main@origin --name-only`) and move each to its new home with
  its imports fixed.
- Main's serials win: take main's manifest versions, contract numbers and lockfile, then
  regenerate the lockfile (`bun install`) and set the copies your branch made (a second manifest
  that must match the first) to the same values. A plan that pinned a number main has since
  moved is overtaken, not departed from; record it in the handoff.
- The merged head is a new head: re-run the real-surface proof and the real-binary suites there,
  on the pinned binary the merged tree names, and record the fingerprint before and after
  (`changed` is the expected answer when main's edits were ported).

## Evidence

sjawhar/legion#1831 moved `extensions/legion.ts`, its tests, `src/legion/*`, `agents/` and two rigs
out of `packages/pi-envoy`. Main moved twice during one implementing round. The first forward merge
(c90774ff39ee, 101 commits) carried 22 conflicts, 12 of them `including 1 deletion`: main's edits
to `legion.ts`, `legion.test.ts`, `pi-types.ts`, `tool-result.ts`, the grant rig and two skills
were ported onto `packages/pi-legion/…` and `packages/pi-shared/…`, main's new
`src/tool-result.test.ts` moved with its module, both manifests took main's 7.11.1, and
`bun.lock` was restored from main and regenerated. The second (ab74aa76d09d) brought LEGION-583's
daemon API contract 13, so `@sjawhar/pi-legion` declares 13 although the plan said the field stays
at 12 — the split bumps nothing; main did. Each merge was one commit from the bookmark
(`jj new legion/LEGION-247 main@origin`), never a rebase; the fingerprint comments on the pull
request record `changed` both times, and `legion probe-image` was re-run at each merged head
(`daemon-api-version=12`, then `13`).
