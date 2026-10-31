---
title: "A forward merge onto main's competing rule keeps main's code and deletes your copy and its tests"
category: legion
tags:
  - forward-merge
  - conflicts
  - duplicate-tests
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2366"
---

# A forward merge onto main's competing rule keeps main's code and deletes your copy and its tests

Extends docs/solutions/legion/a-stopgap-that-lands-on-main-mid-plan-is-superseded-not-merged.md.

When main merges its own version of a rule your pull request also implements, and the architect
decides that main's rule wins:

- Keep main's functions and their tests unchanged. Delete your copy of the rule. Rebuild on top
  of main only what main lacks, and send each recipient one copy alongside main's.
- Delete your tests that only repeat main's, so a later change to the rule has one place to
  update. Keep a test only when it covers something your pull request adds.
- Read every comment, doc line, changelog fragment and benchmark baseline your pull request wrote
  about the old main, and rewrite each one against main as it now is. A baseline measured before
  main's change is labelled as that tree, with its comparison in the past tense.
- When aligning with main leaves a field or helper of yours with no reader, search every reader in
  the repository, record the search in the pull request's contract change census, and delete it.
- Expect the fingerprint to change and a full test round to follow. Re-run your delivery proof on
  the merge, and run the same proof against main's own build as the negative control.

## Evidence

sjawhar/legion#2366 and #2225 both added a claimant route to Dispatch's outbox. The merge
06af8ab7/be2fe717 kept #2225's `routing` and `publishClaimantRoute`. Review rounds then removed
three tests that duplicated `publisher_claimant_test.go` (55be984b), relabelled `THROUGHPUT.md`'s
baseline as main before #2225, and deleted `suppressed_authors`, which nothing read once the routes
read `mentions` (ed6e225d). On main's own build, the delivery rig failed exactly the 5 cases this
pull request adds.
