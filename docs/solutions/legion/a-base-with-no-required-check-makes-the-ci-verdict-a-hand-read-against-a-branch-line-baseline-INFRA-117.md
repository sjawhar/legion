---
title: "A base with no required check makes the CI verdict a hand-read, named job by job, and a red is classified against a baseline on the same branch line"
category: legion
tags:
  - required-checks
  - ci
  - merge-gate
  - long-lived-branch
date: 2026-10-22
status: active
module: legion-worker
related_issues:
  - "INFRA-117"
  - "sjawhar/legion#2104"
---

# A base with no required check makes the CI verdict a hand-read against a branch-line baseline

Extends `docs/solutions/legion/controller-gate-2-required-checks-live-reads.md`: that document
says how to read a base's required checks live. This is what the roles do when the read comes
back empty, which is the normal case for a long-lived deployment branch nobody protected.

- **Read it, and say it on the pull request.** `gh api repos/<owner>/<repo>/rules/branches/<base>`
  returning `[]`, with no `protection.required_status_checks` either, means the daemon judges no
  head red on that base: a red job never sends the issue back, and READY is accepted with a note
  that the base requires no check. The merge then lands on acceptance with nothing on GitHub in
  the way.
- **Name the whole job list in the plan, and quote every job with its conclusion and run id** at
  the CI head — the passes included, not only the failures. A job nobody named is the failure mode
  here: an unnamed red is indistinguishable from an unrun lane. The implementer writes the list,
  the reviewer re-reads it before approving, and the merger re-reads it at the READY head.
- **Classify a red at the forge, not from a body claim.** A red is pre-existing only when the
  *same job fails at the same step* on a baseline head of the same branch line — the last merged
  head on that base is the baseline to use. `gh api repos/<o>/<r>/actions/jobs/<job id> --jq
  '[.steps[] | select(.conclusion=="failure") | .name]'` on both heads is the comparison; anything
  else is fixed in this pull request.
- **A handoff-only head has no checks at all.** A push carrying only `.legion/` ends its message
  with `skip-checks: true`, so GitHub starts no run for it and the pull request's head shows zero
  check runs. The verdict that counts is the code head's, and the claim that joins them is
  `jj diff --from <code head> --to <handoff head> --git '~.legion'` being empty. Say both SHAs
  wherever the CI line is quoted, or the next role reads "no checks" as "not run".

## Evidence

INFRA-117 merged onto `envoy-infra-integration`.
`gh api repos/sjawhar/legion/rules/branches/envoy-infra-integration` → `[]`, and the branch
carries no classic protection, so the plan named twelve jobs (`lint`, `typecheck`, `test`,
`pr-title`, `build`, `changes`, `envoy-go`, `envoy-infra-go`, `envoy-go-race`, `envoy-smoke`,
`envoy-e2e-local`, `dispatch-memory`) as the verdict the roles enforce by hand. All twelve were
`success` at the code head `7424e75ad6a1`, each quoted with its run and job id in the pull request
body.

One job was red: `dispatch`, job 119658599066, step 13 "Test dispatch dashboard". The same job
fails at the same step 13 on the baseline — #2089's merged head
`c00fcb28eab9a3b4831007e2cd0996ef1f044021`, job 119082906542 — and this pull request touches no
file under `packages/dispatch`, so it was recorded as pre-existing with both run ids on the body
rather than chased. The tester and the reviewer each re-read both jobs at the forge and reached
the same classification independently; the architect read them again before signing off.

The pull request's head at review time was a handoff-only push (`e0234413`, then `1987f4a4`,
then `e13e7b0d`), each with zero check runs, and each round's handoff records the code head whose
verdict it quotes.
