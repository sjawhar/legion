---
title: "A flake in a test your branch does not own is read on main and in the open pull requests first"
category: legion
tags:
  - sibling-pull-requests
  - flaky-tests
  - forward-merge
  - conflicts
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2641"
  - "sjawhar/legion#2665"
---

# A flake in a test your branch does not own is read on main and in the open pull requests first

Extends docs/solutions/legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md,
which covers the plan-time trigger, and
docs/solutions/legion/mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md,
which covers the conflict-time one. Neither covers this trigger: a required lane going red in a
test your change did not author.

## The rule

- **Before you write a line against such a flake, read the file's log on the base since your fork
  point and list the open pull requests that touch it.** A flake a loaded runner shows you it
  shows everyone, so the first worker to meet it may already have diagnosed it, and that work
  carries a measurement you would otherwise redo.

  ```bash
  git -C "$LEGION_WORKSPACE" log --oneline "$(git merge-base HEAD origin/main)"..origin/main -- <the test file>
  gh pr list --repo <owner>/<repo> --state all --search "<the test's name>" --json number,state,title
  ```

- **Hand the fix over rather than racing it.** Two branches repairing one test meet as a conflict
  in that file, and the merge decides which repair survives whatever either branch spent. Where
  the sibling is open, say so on its implementer's role topic and wait for it to land; where it
  has landed, merge the base forward and take its version.
- **When the sibling's version is the stronger one, resolve to it whole and keep only the part of
  yours that is still its own argument.** Not "mine plus theirs": the surviving fragment needs a
  reason that stands against the base's version alone, and it needs its own controls, because the
  controls written for your version do not transfer (below).
- **Adopting a sibling's test shape re-derives which mutation exercises which guard.** Main's
  rewrite reports a stopped loop as a *sequence* mismatch, so the hang guard this branch kept is
  reachable only by a run that records nothing at all — a fourth control that did not exist while
  the guard was reported by its own bound.

## Evidence

- sjawhar/legion#2641 spent two implementing rounds on
  `packages/envoy/internal/dispatch/redeliver/sweep_lease_test.go`'s wall-clock flake (a widened
  allowance, then a count of the jitter draws) while sjawhar/legion#2665, opened 2026-11-02, was
  rewriting the same test against the same flake. #2665 merged first and the two met as GitHub's
  `CONFLICTING` in that one file.
- #2665's body names three tests that "failed in this pull request's CI with the code unchanged",
  two of which are two of the three reds that opened #2641's rounds — a diagnosis and a
  measurement table the search would have handed over.
- The resolution: #2641's merge `225b49b06589` restored the file to main's version byte for byte
  and kept one commit on top, `sweeps * 10 * interval` in place of `sweeps * 3 * interval`,
  re-argued against main's own 12 s rather than against the branch's history; its fourth control
  blocks `Run` before its first sweep and fails the guard at 41.7 s, naming the 40 s it gave.
