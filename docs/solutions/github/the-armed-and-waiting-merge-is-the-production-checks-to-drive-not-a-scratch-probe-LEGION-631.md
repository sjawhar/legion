---
title: "The armed-and-waiting merge is the production check's to drive, not a scratch probe"
category: github
tags:
  - auto-merge
  - merger
  - production-check
  - negative-control
  - smoke-repository
date: 2026-10-10
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# The armed-and-waiting merge is the production check's to drive, not a scratch probe

Extends docs/solutions/github/arm-a-github-merge-only-after-your-own-gate-accepts-gh-pr-merge-auto-is-a-merge-not-a-flag-and-disable-auto-dequeues-nothing-LEGION-631.md.

- Of `gh pr merge --auto`'s three outcomes — merged at once, refused, armed and waiting on a
  BLOCKED pull request in a repository that allows auto-merge — a scratch repository without
  auto-merge reaches the first two and never the third, and the third is the one a tester would
  have to arm against a product repository's own branch protection. That is a write against the
  product's rules, not a probe: it is unreached in testing, recorded as such in the tester's
  handoff and the body's `Not proven / risk`, and the production check drives it on the first real
  pull request the merger submits under the new prompt after the pin.
- What a scratch repository does prove, record by its exact refusal text: `Auto merge is not
  allowed for this repository`, `Head branch was modified` for a stale `--match-head-commit`, the
  merged-at-once path when the rules are already met, and `--disable-auto`'s exit 1 with nothing
  armed. A fake that invents these strings pins a guess; the library's branching is what the fake
  is for.

## Evidence

sjawhar/legion#1843: the tester's round 7 drove the merger's command as the review App in
`sjawhar/legion-smoke` (`allow_auto_merge=false`, pull requests #538–#541) and round 8 recorded
the armed-and-waiting path as unreached (`.legion/LEGION-631/test.json`, `failures[1]`); the
architect ruled it the production check's (envoy 6b16770e), and the reviewer's round 4 carried it
as `Not proven / risk` into the approval.
