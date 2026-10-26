---
title: "A list instrument answers about its own subject: check its row count against the forge's own total before scoping anything from it"
category: github
tags:
  - gh-cli
  - pagination
  - evidence
  - measurement
date: 2026-10-25
status: active
module: gh, jj
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# A list instrument answers about its own subject: check its row count against the forge's own total before scoping anything from it

Extends docs/solutions/github/a-green-lane-name-is-not-a-run-read-the-run-and-page-the-check-list-INFRA-31.md.

That document pages a check list, where `gh api` truncates at 30 and `--paginate` is the remedy.
The delta: **`gh pr view --json files` is not an `gh api` call and `--paginate` does not reach it** —
it is a GraphQL query with a fixed cap, and it returns a short list with no error and no marker.
And the inverse failure exists too: a two-dot diff returns a list that is *long* by the base's
churn. Same wrong shape, opposite direction, depending on which way the comparison runs.

- **Scope from the forge's own three-dot list and check its row count against `changedFiles`.**
  `gh api --paginate repos/<owner>/<repo>/pulls/<n>/files --jq '.[].filename'`, compared with
  `gh pr view <n> --json changedFiles`. If they disagree, the list is short and nothing follows
  from it.
- **`jj diff --from <base>@origin --to <head>` is two-dot and includes the base's own commits.**
  The three-dot equivalent is `jj diff --from "fork_point(<base>@origin | <head>)" --to <head>`,
  which matches the forge's list exactly.
- **Before acting on a field, say in one line what it is about**: its subject, whether it records
  what was asked for or what happened, and the command it came from. A reading that is correct
  about something else is indistinguishable from the reading you wanted.
- **A second source only helps when it is about the same subject.** Three sources that each answer
  a question nobody asked are worse than none: the cost of the rule is paid and its benefit is not,
  and the claim now *looks* checked.

## Evidence (INFRA-80, sjawhar/legion#2173)

At one head of that pull request, measured together:

```
gh pr view 2173 --json changedFiles                  -> 141
gh api --paginate .../pulls/2173/files | wc -l       -> 141
gh pr view 2173 --json files --jq '.files|length'    -> 100
jj diff --from 'main@origin' --to <head> --name-only -> 328
jj diff --from 'fork_point(main@origin | <head>)' …  -> 141
```

`--json files` was short by 41 paths, silently. The two-dot diff was long by 187 — the base's own
71 commits of churn — which is safe but noisy, and it collapses to the forge's number once the base
is merged forward, which is itself a usable check that a merge did what it looks like.

Three more instances of the same shape in one change, each a correct reading of another subject:

- A run's creation time read from the attempt endpoint rather than the run object: the right number
  about the wrong subject, which is indistinguishable from a false figure.
- A negative control reported as "eight failing cases" where one was a behaviour difference and
  seven differed only in a refusal message's wording.
- A claim that a Go package "cannot run in this pod", backed by three independent readings — no
  `docker` on `PATH`, `DOCKER_HOST` unset, no socket — every one of them a correct reading of
  *"this pod has no container runtime"*, which nobody disputed. The field that held the answer was
  the test's own skip text, and it named `LEGION_TEST_PG_DSN`, a database. The package ran in four
  seconds once a Postgres was pointed at it.
