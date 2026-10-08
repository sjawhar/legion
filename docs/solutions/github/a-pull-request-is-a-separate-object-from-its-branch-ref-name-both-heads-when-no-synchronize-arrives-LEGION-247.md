---
title: "A pull request is a separate object from its branch ref: when a push starts no run and the pull request is not conflicting, name both heads"
category: github
tags:
  - github-actions
  - pull_request
  - synchronize
  - webhooks
  - headRefOid
  - git-ref
  - ci-watcher
date: 2026-10-07
status: active
module: .github/workflows, packages/daemon
applies_when:
  - `gh run list --branch legion/<KEY>` shows no run for a commit you pushed and `mergeStateStatus` is not DIRTY
  - `pr view --json headRefOid` answers a commit behind the one `legion push` moved the bookmark to
  - A CI, E2E or review line must name the head GitHub built
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A pull request is a separate object from its branch ref

Extends docs/solutions/github/conflicting-pr-gets-no-pull-request-ci.md.

- A pushed commit with no check suite on a pull request whose `mergeStateStatus` is not `DIRTY`
  is the pull request lagging its branch: GitHub updates the pull request's head from a
  `synchronize` event its push delivers, and under a Webhooks or Pull Requests outage the ref
  moves while the pull request does not. Diagnose it with two reads, never a wait on `run list`:
  `api repos/{o}/{r}/git/ref/heads/legion/<KEY>` against `pr view <n> --json headRefOid`.
- Do not re-push the same commit and do not wait it out: the next genuine push (the following
  phase's handoff commit) synchronises the pull request and carries the skipped commits with it.
  A push of only `.legion/` handoffs carries `skip-checks: true`, so the code head's verdict is
  the one the daemon carries either way.
- State CI by the head GitHub built and name every commit above it that touches nothing but
  `.legion/`: an E2E, review or merge-gate line naming a head no run exists for is unverifiable
  by the next role.

## Evidence

sjawhar/legion#1831: the implement handoff commit 84d0a35c was pushed at 15:10Z during GitHub's
2026-10-07 outage. At 16:01Z the tester read `git/ref/heads/legion/LEGION-247` = 84d0a35c while
`pr view` answered `headRefOid` 013b1415 (27 commits), `mergeStateStatus` CLEAN, with no
`synchronize` event, check run or workflow run for 84d0a35c; the two `PR Title` runs at 15:17Z
and 15:44Z were `edited` events at 013b1415. The tester's own handoff push synchronised the pull
request (`headRefOid` d3495d09, 29 commits, 84d0a35c among them). The reviewer's round-4 facts cite
CI at the code head 013b1415 (Tests 37642167306, Legion Envoy and Contracts 37642167376, Worker
Image 37642167778 with `probe-image: OK … daemon-api-version=14`, Docs 37642167241) and name the
three `.legion/`-only commits above it, and the daemon carried that head's `checksVerdict: green`
to the approved head 355526e5.
