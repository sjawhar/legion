---
title: "When a stacked pull request's base squash-merges and the pull request is retargeted: the unchanged-diff fingerprint's before side is against the base's last SHA, and the forward merge conflicts in files the branch never touched, which take main's version"
category: legion
tags:
  - stacked-pr
  - squash-merge
  - forward-merge
  - fingerprint
  - conflict
  - jj
date: 2026-10-08
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-630"
  - "sjawhar/legion#1845"
  - "LEGION-462"
  - "sjawhar/legion#1752"
---

# When a stacked pull request's base squash-merges and the pull request is retargeted

Extends docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md.

- Once the base branch a pull request was stacked on has squash-merged and GitHub has deleted
  its bookmark, `fork_point(main@origin | <old tip>)` is no longer the base the branch was cut
  from: it falls back to where the stacked base itself forked from `main`, and the before
  fingerprint then includes the base's own commits. Take the before side against the stacked
  base's last SHA instead — `fork_point(<base sha> | <old tip>)` — which the shared clone still
  resolves after the bookmark is gone; the `Chain` line of the PR body is where that SHA was
  recorded when the stack was formed. The after side is against `fork_point(main@origin | <new
  tip>)` = `main@origin`, as the reference says.
- Expect the forward merge of `main@origin` to conflict in files the branch never touched: the
  three-way merge's base is the stacked base's own fork point, `main`'s side carries the whole
  base pull request as one squash, and the branch's side carries the base's commits up to the
  head it merged, so every file the base kept changing after that head differs on both sides
  (a file the base added after that head included). Each such file takes `main`'s version
  (`jj restore --from main@origin -- <paths>`); the branch has no line in them, and the retro's
  diff and the fingerprint both show that nothing of the branch moved.
- A file the branch did touch conflicts the same way when the base also kept changing it. Take
  `main`'s text and re-apply the branch's own hunks by hand; read them first from
  `jj diff --from <base sha> --to <old tip> --git -- <file>`, which lists exactly the lines that
  are the branch's.

## Evidence (LEGION-630, sjawhar/legion#1845)

The branch was stacked on `legion/LEGION-462-issue-pod` (#1752) by a forward merge of its head
`42a685d1` (`bcde255e`), with the pull request's base that branch. #1752 then took more fixes and
squash-merged to `main` as `bb51895b`; GitHub retargeted #1845 to `main` and showed it
`CONFLICTING`. The daemon sent the implementer back to implementing with "the head conflicts
with main: GitHub runs no checks on it; merge main forward".

Fingerprints at the old tip `dda891d4` (the one-fileset command, `'~(.legion | docs/solutions)'`):
against `fork_point(main@origin | dda891d4)` = `ae1f44b7` it was `7883a8aa…`; against
`fork_point(42a685d1 | dda891d4)` = `42a685d1` it was `72d72b28…`. After the merge `569f2248`,
at the new tip `55b5133e` against `fork_point(main@origin | 55b5133e)` = `41c4d814`: `72d72b28…`.
The first number is not the branch's diff: it is the branch plus #1752's commits that
`main` had not yet carried at `ae1f44b7`. The tester computed the same two `72d72b28…` figures
independently and the reviewer confirmed the head by SHA; both would have called a `7883a8aa… →
72d72b28…` pair a changed diff and started a round.

Eleven files conflicted. Eight the branch never touched — `docs/kubernetes.md`,
`packages/daemon/internal/launcher/launcher_test.go`, `internal/runtime/runtime.go`,
`internal/runtime/sandbox/{launcher,relaunch,relaunch_test}.go`, `internal/supervise/machine.go`,
`scripts/e2e/lib/workflow.sh` — and each is a file #1752 changed between `42a685d1` and its
squash (`jj diff --from 42a685d1 --to main@origin --stat` over them: 540 insertions, 122
deletions; `launcher_test.go` and `relaunch_test.go` were born after `42a685d1`), while
`jj diff --from 42a685d1 --to dda891d4 --summary` over the same paths printed nothing; they took
`main`'s version. The three the branch did touch (`AGENTS.md`, `scripts/e2e/README.md`,
`scripts/e2e/stage4b-sandbox-tree.sh`) took `main`'s text with the branch's one-to-three-line
hunks re-applied. `jj diff --from main@origin --to 569f2248 --stat` then listed exactly the
branch's own nineteen files, and the retro documents committed before the conflict (`dda891d4`)
rode below the merge unchanged.
