---
title: "A check on main warns and goes on, and its strict form refuses in a required job"
category: best-practices
tags:
  - ci
  - required-checks
  - release-push
  - changelog
date: 2026-10-31
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2411"
---

# A check on main warns and goes on, and its strict form refuses in a required job

Extends docs/solutions/best-practices/a-required-check-runs-mains-post-merge-command-and-writes-nothing-in-the-checkout-pr2423.md.

- When a job on main processes many independent inputs (packages, fragments, files), one bad input must not stop the others.
  On main the shared command names the bad input in one `::warning::` and processes the rest, exiting 0.
- Give the same command a strict mode (a subcommand of its own, sharing the collector and the message) that exits 1 on the bad input, and run it in a job the base branch's ruleset requires.
  Read the ruleset (`gh api repos/<owner>/<repo>/rulesets/<id>`) to find which contexts are required; in sjawhar/legion that is `ci-ok`, so the job must be one `ci-ok` needs.
- A workflow of its own with a `pull_request.paths` trigger is not a gate: its job is a separate check run that no ruleset names.
- Keep the input list both modes read in one file of its own (not a workflow's `env:`), so the job on main and the required job cannot read different lists.
- Test both modes on one fixture: the strict mode refuses, and the main mode still prints the plan for every good input.

## Evidence

sjawhar/legion#2411 first made `release-push.sh fold-plan` refuse a fragment under the `.changes/` of a package the fold does not fold (`7fbb983f`).
The tester found two faults at once: the refusal ran only in `changelog-fold.yaml`'s `fold_build`, which no required context needs, so it gated no merge; and on main one such fragment made `fold-plan` exit 1, so no package was folded.
The fix (`ea39694f`): `fold-plan` warns and plans the rest; `release-push.sh fold-orphans` refuses in `ci.yaml`'s `lint` job; both read `.github/changelog-packages`.
