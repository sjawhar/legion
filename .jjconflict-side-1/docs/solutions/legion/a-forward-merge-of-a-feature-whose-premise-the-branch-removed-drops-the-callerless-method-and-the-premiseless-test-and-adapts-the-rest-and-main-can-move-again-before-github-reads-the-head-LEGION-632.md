---
title: "A forward merge of a feature whose premise the branch removed drops the callerless method and the premiseless test, adapts the rest, and expects main to move again before GitHub reads the head"
category: legion
tags:
  - forward-merge
  - jj
  - conflict-resolution
  - daemon-api-contract
  - github-merge-check
  - re-authored-working-copy
date: 2026-10-09
status: active
module: packages/daemon
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# A forward merge of a feature whose premise the branch removed drops the callerless method and the premiseless test, adapts the rest, and expects main to move again before GitHub reads the head

Extends docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md.

- "Keep both sides" stops at an addition whose premise your branch removed. Check the callers of
  every method or field `main` added in a conflicted file against the merged tree: a method whose
  only caller your branch deleted is removed at the merge, from the interface and every
  implementation, not kept for symmetry. `main`'s `SessionsOnVolume()` existed so `Retree` could
  drop a session that lived on the tree's volume; on a branch where the session is the issue's and
  `Retree` keeps it under every runtime, the method had no caller and went (`internal/runtime/runtime.go`,
  the fake, tmux and sandbox runtimes, round 9 of #1842).
- Adapt `main`'s new tests to your branch's shapes; drop only a test whose premise is gone. #1860's
  `session_store_test.go` called helpers with an affinity flag this branch removed and asserted
  `LEGION_EXPECT_TREE_VOLUME` with a tree-keyed fake store: ported to the three-argument helpers,
  `LEGION_EXPECT_ISSUE_VOLUME` and the issue-keyed store, every row kept. #1853's
  `TestARelaunchWaitingOnItsTreeEndsWithItsContext` asserts a launch stops waiting on its tree's
  other pods — a wait this branch removed with the tree launch turn — and was dropped, its feature
  (cancel-on-stop) landing untouched. Say which in the fingerprint comment.
- Port `main`'s present-tense prose about the component your branch changed, in code comments and
  test messages too, and leave its history alone: #1860's `SessionDSNKey`, `resumable` and
  `Created` docs said "the tree volume" of the current release and were ported; its `legion-v10.0.0`
  import runbook says "tree volume" of that release and stays. Two sentences missed at the merge
  (a Go doc and a live test's messages) cost a reviewer's ask and one more commit.
- A head GitHub reads `CONFLICTING` runs no `pull_request` workflow at all, and GitHub reads the
  head against the `main` of the moment it computes the merge ref, not the one you merged. After
  pushing a forward merge, fetch and read `mergeable` again (`legion gh -- pr view --json
  mergeable,mergeStateStatus`) about thirty seconds later; when `main` moved meanwhile, merge again
  at once with `jj new legion/<KEY> main@origin` rather than waiting for a round to be sent back.
  #1842's round 9 needed two merges minutes apart (#1860, then #1853); the first head got no run.
- Re-read `main`'s `DaemonAPIVersion` at every forward merge and renumber only when it moved past
  yours (docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md):
  #1842 renumbered 16 → 17 → 18 at two of four merges and stayed at 18 for the last two, since
  pi-legion 8.7.0 still declared 17.
- A commit you push after your phase completed is authored by the next role's App: the daemon
  re-authors the working copy for the role whose phase is active, so an architect-directed
  docs-only commit above the handoff head lands under that App's name. Acceptable when the
  architect says so; name it in the PR body's `Chain` line, and expect `legion gh` to refuse with
  `GRANT_EXPIRED` once your phase is over (read a published image digest from GHCR's anonymous
  manifest API, `/v2/<image>/manifests/sha-<commit12>`, when you still owe it to someone).

## Evidence

#1842 (LEGION-632) went through nine implementing rounds, four of them forward merges GitHub
forced by reading the head `CONFLICTING` against a `main` that had gained #1846/#1857 (round 6),
#1837 (round 7), #1854 (round 8) and #1860 then #1853 (round 9); fingerprints before and after each
are in the PR's comments (6076723481, 6080268138, 6083808917, 6087904205, 6088044574). Round 9's
thirteen conflicts kept #1860's Postgres session store beside this branch's per-issue volume,
per-role state home and reservation; the resolutions are listed file by file in
`.legion/LEGION-632/implement.json` and the two fingerprint comments. The tester's observation at
the merged head named the two "tree volume" sentences and the Volume retention paragraph the
merge left unqualified; the reviewer asked for one docs-only commit (f089af14) rather than a
round, and approved that head.
