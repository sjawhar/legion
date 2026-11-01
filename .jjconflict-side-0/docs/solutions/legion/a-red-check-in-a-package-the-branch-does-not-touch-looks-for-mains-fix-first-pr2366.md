---
title: "A red check in a package the branch does not touch looks for main's fix first"
category: legion
tags:
  - ci
  - forward-merge
  - flaky-test
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2366"
---

# A red check in a package the branch does not touch looks for main's fix first

When a required check is red on a test in a package your pull request does not change, do this
before you investigate the test:

1. Confirm the branch does not change the package: `git diff --stat <fork point> <head> -- <package>`
   prints nothing.
2. Look for a fix on main since your branch's base:
   `git log --oneline <fork point>..<main> -- <package>/<test's directory>`.
3. When GitHub also reports the pull request conflicting, the forward merge is required anyway and
   brings main's fix. Make it, then run the failing tests on the merge, with and without `-race`,
   before you push. When the pull request is not conflicting and main has no fix, re-run the failed
   job once instead of pushing code.
4. Record in the pull request comment that carries the merge's fingerprints that the red was outside
   the change, which main commit fixed it, and the local result.

## Evidence

At 16191b48, the `test` check of sjawhar/legion#2366 failed on `TestTwoDeployRunsApplyingAfterTheMergeWakeTwoLooks`
and `TestTheSameDeployRunDeliveredTwiceWakesOneLook` in `packages/daemon/internal/daemon` under
`-race` (job 122325058297). The branch changed nothing under `packages/daemon`. Main had fixed the
rig in 2dbeca18b ("the check_wake rig uses the host workspace, not a feed"). The pull request also
conflicted with main, so the forward merge fb909998 brought the fix, and both tests passed on it
locally with and without `-race`. A later red in `packages/daemon/internal/intake`, with no fix on
main and no conflict, was re-run once and passed.
