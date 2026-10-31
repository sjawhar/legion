---
title: "Zero check runs on a pushed head is a conflicting head, not a skip-checks trailer"
category: github
tags:
  - ci
  - pull-request
  - triage
  - merge-conflict
date: 2026-10-30
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2504"
---

# Zero check runs on a pushed head is a conflicting head, not a skip-checks trailer

Two independent conditions give a pushed head **no** workflow run at all, and they look identical
from `gh api .../check-runs`: the count is 0 and nothing appears under the branch's runs. Triage
them in this order, before you push anything else.

1. **Read the pull request's mergeability first:**
   `gh pr view <n> --json mergeable,mergeStateStatus`. `CONFLICTING`/`DIRTY` means GitHub computed
   no merge ref for the head, so it runs no `pull_request` workflow for it. The repair is the
   sanctioned forward merge of the base; nothing about the commits is wrong.
2. **Only then read the head commit's message** for `skip-checks: true` or a bracket keyword.
   GitHub reads that trailer on the **head of the push**, so a push that carries code commits
   without it and ends on a handoff commit with it starts nothing for any of them.

A push whose head carries no trailer and still produces no run is condition 1 by elimination: that
elimination is cheaper than a second push.

## Evidence

In sjawhar/legion#2504 a round ended with 0 check runs at its code head, and the first diagnosis
was the trailer, because the branch's handoff commit carries one. Two further pushes made to test
that theory — an empty commit and a handoff commit, **neither carrying the trailer** — also
produced 0 check runs, while the repository's Actions were demonstrably running for other branches
in the same minutes. `gh pr view 2504 --json mergeable,mergeStateStatus` then answered
`CONFLICTING`/`DIRTY`: main had moved under the branch. The forward merge that cleared it produced
`CI` and `PR Title` within a minute of the push.

Both conditions are real in this repository, and both were in play in that round: the trailer rule
holds for every push that carries code (end it on a commit without the trailer, and push the
handoff separately), and a conflicting head is the one that makes the rule look broken.
