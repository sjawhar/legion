---
title: "A green lane name is not a run: read the run that produced it, and page the check list"
category: github
tags:
  - ci
  - checks
  - gh-api
  - pagination
  - review
date: 2026-10-21
status: active
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# A green lane name is not a run: read the run that produced it, and page the check list

Two independent ways to be told a head is green when nothing you changed was exercised.

- **The name is a claim about a job, not about your change.** A lane called `test` is green when
  the job it names succeeded — which it also does when the job's path filter excluded your
  package, when it ran on another head, when it skipped, or when the step you care about was not in
  it. Before citing a lane, open the run: its head SHA, its trigger, the step that ran your code,
  and the line in that step's log naming your test. The cheap version:
  `gh run view <id> --log | grep -n '<your test name>'` — the absence of that line is the finding.
- **`gh api` paginates at 30, so a long check list silently truncates.** A pull request with more
  check runs than one page returns a subset, and a required check absent from the page reads
  exactly like a required check that has not reported. Always
  `gh api repos/<owner>/<repo>/commits/<sha>/check-runs --paginate -f per_page=100`, and judge the
  required set (here `lint`, `typecheck`, `test`, `pr-title`) by name against what the page
  actually contains rather than by the summary conclusion.

Both have the same remedy as the general rule in
docs/solutions/testing/evidence-can-be-absent-rather-than-wrong-and-absence-reads-exactly-like-success-INFRA-31.md:
name what would have made this check fail. For a lane, that is "the test would have appeared in
the log and failed" — so go and see the test in the log.

One corollary specific to a long-running branch: a lane that is red for a reason the branch cannot
cause (another package's flake, a registry rate limit) is still not evidence of anything about your
head. Read it, name it, and say which of the required four it is or is not.
