---
title: "A credentialed job runs only the commit the event fixed, and its own script's subcommands are judged one by one"
category: github
tags:
  - release-workflows
  - deploy-key
  - build-publish-split
  - release-push
  - check-release-credentials
  - github-sha
date: 2026-10-28
status: active
module: .github/workflows
supersedes: docs/solutions/github/a-credentialed-release-job-splits-into-a-build-half-that-hands-over-bytes-and-a-publish-half-that-re-derives-names-recomputes-the-version-and-pushes-one-checked-bump-LEGION-748.md
related_issues:
  - "INFRA-629"
  - "sjawhar/legion#2419"
---

# A credentialed job runs only the commit the event fixed, and its own script's subcommands are judged one by one

The LEGION-748 document says the publish half may run "the repository's own release script" and
may take "one scalar" from the build half. INFRA-629 showed both are unsafe as written. Rules 1
and 2 below correct its rules 1 and 2; its rules 3 to 6 stand as written there.

## The rule

1. **The repository's own script is dependency code one subcommand at a time.** A subcommand that
   runs a third-party binary (`release-push.sh fold-notes` runs changie) is dependency code, and
   the credential check must name that subcommand, not exempt the script by path. When you add a
   subcommand to an exempted script, read whether it runs anything that is not this repository's
   code, and add a check case that puts it in the credentialed job and fails.
2. **No build-half output chooses what the credentialed job runs.** The publish half checks out
   `${{ github.sha }}` (the event fixes it: main's tip for `workflow_run`, `schedule` and
   `workflow_dispatch`, the merge commit for `pull_request`), never a `needs.<build>.outputs.*`
   ref. A check inside the script it checks out only checks itself, and `actions/checkout` fetches
   any reachable SHA, fork pull request heads included. The build half may hand over bytes and a
   flag that only decides whether the publish half runs, provided the publish half recomputes its
   own plan from git.
3. **When the publish half recomputes and finds work the build half did not prepare, decide it in
   the spec.** State can move between the jobs (a release tag pushed in the gap). Defer the part a
   race explains, with a `::notice`, and land the rest; refuse only what a race cannot explain.
4. **A comment that says "the check pins X" is tested by putting X back and running the check.**
   `check-release-credentials.sh` reads no checkout `ref:`, so the `github.sha` rule is kept by the
   workflow, and its comment says so.

## Evidence

- Review round 1 of sjawhar/legion#2419: `changelog-fold.yaml`'s fold_publish checked out
  `needs.fold_build.outputs.base`, the output of the job that runs changie, and ran
  `release-push.sh push-fold` from it with the deploy key. Fixed in 6b255451bb62: both jobs check
  out `github.sha` and `BASE: ${{ github.sha }}`.
- The same round: `check-release-credentials.sh` skipped `release-push.sh` as a whole
  (`path != RELEASE_PUSH`), so a `release-push.sh fold-notes` step in fold_publish passed. Fixed in
  6b255451bb62 with a rule that names `fold-notes`, and test cases.
- The tester's red test: a `legion-envoy-v*` tag pushed between fold_build and fold_publish gave
  push-fold's plan a group without notes, and the run failed. Fixed in 0bcd639c45ab: that group is
  deferred with a `Fold deferred` notice; a missing `main@` group, which no race explains, is
  still refused.
- Review round 2: the workflow comment said the check pins the `github.sha` checkout; restoring the
  round-1 `base` output still passed the check. Fixed in 460a6f9e021b.
