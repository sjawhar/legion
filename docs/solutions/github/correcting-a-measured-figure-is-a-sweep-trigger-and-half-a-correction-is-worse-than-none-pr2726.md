---
title: "Correcting a measured figure is a sweep trigger, and half a correction is worse than none"
category: github
tags:
  - ci
  - github-apps
  - review
  - documentation
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# Correcting a measured figure is a sweep trigger, and half a correction is worse than none

Extends docs/solutions/github/moving-a-package-between-jobs-moves-its-reasons-too-and-only-the-implementer-pushes-a-workflow-file-pr2516.md.

That note arms the trap for a package **moved** between jobs: grep every place its reasons live,
and remember that only the implementer can push a workflow file, because GitHub refuses one from
the review App.
Correcting a number arms the same trap, and it is easier to miss, because a correction feels
finished the moment the one copy in front of you is right.

- **Before you change a measured figure, grep the repository for it and for the sentence around
  it.**
  A figure that justifies a deadline, a limit or a package's treatment is usually written twice:
  once in the workflow comment beside the thing it justifies, and once in the guide that explains
  it.
- **Correct every copy in one round, or none of them.**
  A stale figure read in two places is uniformly stale, and a reader who finds it is wrong
  anywhere knows it is wrong everywhere.
  A figure corrected in one place and not the other is two different claims about the same
  measurement with nothing to say which is current, and the reader who trusts the wrong one has
  been given a reason to.
- **When a copy lives in `.github/workflows/`, name in the same round who pushes it.**
  The review App cannot, so a reviewer who fixes the document half has, by doing so, created the
  divergence.
  Either send the whole correction back to the implementer, who can push both, or say on the pull
  request which copy is still to be fixed and by whom — never leave it only in a handoff, which
  the role that can act on it does not read.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) corrected one paragraph of `docs/testing.md` three
times across three roles, over a figure that lives in two files.

- The paragraph said `internal/daemon` "took up to 694 s without `-race`".
  The implementer replaced that with measured figures and, in the same sentence, rewrote
  "306 to 600 s under `-race`" to "306 to 570 s on the green runs of 2026-11-02 and 2026-11-03",
  copying the wording from `.github/workflows/ci.yaml`'s race-step comment (its lines 1908-1911).
- The tester measured that population: over the 39 green main `test` jobs of those two days the
  range is 322.432 s to 560.390 s, so neither end is attained.
  The figure is from sjawhar/legion#2709's pull request body, over 51 green `test` jobs of those
  days with pull-request jobs in the set.
- The reviewer fixed `docs/testing.md` itself, in a prose-only commit naming that population, and
  recorded in its approval that `ci.yaml`'s copy "is not in this diff".
  It could not fix that copy: GitHub answers a workflow write from the review App
  `refusing to allow a GitHub App to create or update workflow .github/workflows/ci.yaml without
  workflows permission`.
- So at the approved head the document names the 51-job population and the workflow comment ten
  lines from the step it justifies still names "the green runs of 2026-11-02 and 2026-11-03".
  Retro cannot close it either: a retro commit may touch only `docs/solutions/`.
