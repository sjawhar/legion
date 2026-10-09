---
title: "A stacked branch whose base squash-merged conflicts with its own content: resolve and fingerprint against the old base's last commit, not jj's fork point, and re-check what jj auto-merged"
category: legion
tags:
  - jj
  - forward-merge
  - stacked-pr
  - squash-merge
  - retarget
  - fingerprint
  - fork-point
  - conflict-resolution
date: 2026-10-08
status: active
module: skills/legion-worker
applies_when:
  - A pull request was opened against another issue's branch, that branch's pull request squash-merged into main, and GitHub retargeted yours to main
  - The retargeted head shows CONFLICTING and GitHub runs no checks on it
  - The unchanged-diff fingerprint must be computed for a branch whose old base bookmark no longer exists
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
  - "LEGION-462"
  - "sjawhar/legion#1752"
---

# A stacked branch whose base squash-merged conflicts with its own content

Extends `docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md`:
that document's procedure takes the fork point from `fork_point(main@origin | <head>)`, and the
skill's reference says to substitute the base branch for `main` on a stacked pull request. Once the
base's pull request has squash-merged and its bookmark is deleted, neither works as written.

- **Expect every file the base changed to conflict, not only the files you changed.** The base's
  commits are in your branch's ancestry; main has their content as one squash commit with none of
  them, so jj's fork point for `main | head` falls back to the main commit the base last merged,
  and the base's own edits appear on both sides of the merge. Seventeen files conflicted on
  LEGION-629; this branch had touched eleven of them.
- **Classify each conflict by three hashes before resolving**: the file at the old base's last
  commit (`B`, the base-branch tip your branch had merged), at `main@origin` (`M`), and at your tip
  (`T`). `M == B` → main added nothing after what you merged: take yours. `T == B` → you never
  touched it: take main's (the base's later commits). Otherwise both sides moved: three-way merge
  with `git merge-file` against `B`, which found no overlapping hunks in all five such files here;
  a hunk that does overlap is the one place to read and resolve by hand.
- **Audit what jj resolved on its own with the same three hashes.** jj merges the non-conflicting
  files against its own fork point, which predates the squash, so a file where only you had edited
  (`M == B ≠ T`) can come out as *main's* version, silently reverting your change:
  `podsafety_test.go` came back with the three tests this branch had collapsed, and only
  `go vet` (`undefined: overlay`) showed it. Run the classification over every file the merge
  commit changed relative to your tip, not only the ones jj listed as conflicts.
- **Fingerprint against the old base's last commit on both sides, and say so.** `--from <B>`
  (`42a685d1` here, found from the base pull request's merge-time head or your own earlier
  "merge base into branch" commit's second parent) gives the same digest before and after the
  merge; `fork_point(main@origin | <old head>)` counts the base's whole diff as yours and differs
  from the post-merge value, which would read as a changed diff and force a full test round for
  nothing. The reviewer independently reached the same base from `fork_point(<old base tip> |
  head)`; the rebase comment and the handoff should name the base commit and why.
- **Once the pull request targets main, workflows gated on `pull_request.branches: [main]` run
  for the first time.** The worker image's build-time probe (`Worker Image`) never ran while the
  pull request targeted the stacked base; it ran at the merge head and was acceptance 4a's real
  surface. Check the PR's check list after the retarget for what newly applies.

## Evidence

- LEGION-629 (PR #1848) was opened against `legion/LEGION-462-issue-pod` (#1752). #1752
  squash-merged as `bb51895b`, GitHub retargeted #1848 to `main`, and head `75e013dd` showed
  `CONFLICTING` with no checks. The forward merge `137de544` resolved 17 conflicts: 7 taken as the
  branch's (one, `podsafety_test.go`, after jj's auto-merge had taken main's), 6 as main's, 5 by
  `git merge-file` against `42a685d1` with no overlapping hunks. Fingerprint `bba40d23…` before
  and after against `42a685d1`; `e21a05c7…` against jj's `fork_point(main | 75e013dd)` =
  `ae1f44b7`. The reviewer's confirmation (round 3) recorded the same two fork points and the same
  reasoning.
- `Worker Image` run 37849989804 ran on the merge head as a `pull_request` run and printed
  `probe-image: OK … extensions=discovered … daemon-api-version=15`.

## Related

- `docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md` — the
  fileset and what a rebase may change.
- `docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md`
  — another case where the merge needs reading beyond jj's conflict list.
