---
title: "Zero runs at the head you just pushed is read with mergeability or not at all"
category: github
tags:
  - github-actions
  - pull-requests
  - merge-conflicts
  - ci
  - legion-worker
date: 2026-10-25
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-251"
  - "sjawhar/legion#2163"
---

# Zero runs at the head you just pushed is read with mergeability or not at all

Extends `docs/solutions/github/an-empty-check-set-is-a-conflict-until-proven-otherwise-INFRA-42.md`,
which tells whoever is **waiting** on checks to treat an empty run set as a conflict until proven
otherwise. The delta is for the role that is forbidden to wait:

- **One read is all a non-waiting role gets, so it must be the two-field read.** A Legion
  implementer pushes and completes; it never polls CI. Its single `gh run list --commit <head>` at
  completion answers the same empty list for "the push landed ten seconds ago" and for "no run will
  ever exist here". Read `mergeable,mergeStateStatus` in the same breath, every time, and never
  write an empty run set into the pull request body without it.
- **`no run yet` is a claim about the future, and only `MERGEABLE` licenses it.** With
  `CONFLICTING`, the honest line is that the head will never be judged — which is a stop, not a
  completion.
- **Nothing downstream recovers it for you.** The daemon reads settlements, not absences; a red
  check sends the work back, a missing check does not. The round is handed on, the tester reads the
  same empty list, and the conflict is found one phase later at the cost of a full round.
- **The conflict can arrive between the push and the read.** A sibling merging into the base while
  the round runs is what makes the head conflicting; the push that created the head is no evidence
  about it.
- **Cure with the forward merge, never a rebase**
  (`skill://legion-worker/references/conflicts-and-rewrites.md`), and post the before and after
  fingerprints; a merge that brings the base's files in changes the fingerprint, so the earlier
  evidence does not carry by fingerprint alone.

## Evidence (INFRA-251, sjawhar/legion#2163)

Round 2 pushed head `eb4e98d5da36fde43138c19b1e31710c34f127c2`, read
`gh run list --commit eb4e98d5…` once at completion, found nothing, and wrote `no run yet` into the
body's `CI` line on the reasoning that the push had landed seconds earlier. It had not landed
seconds earlier into a mergeable pull request: `main` had moved under the branch and GitHub showed
the pull request `CONFLICTING`/`DIRTY`, so it queued no `on: pull_request` run and
`gh api repos/sjawhar/legion/commits/eb4e98d5…/check-runs --jq .total_count` answered `0`. The whole
round — a harness fix and its three tests — was judged by no lane.

The tester found it and failed the round for exactly that: "No workflow run exists at head
`eb4e98d5…` or at round 2's code commit `db300b63…`, and none will start: the pull request conflicts
with main." Round 3 cured it with one forward merge of `main@origin` at `9e62d7451f2c` (one
conflict, `packages/pi-legion/CHANGELOG.md`, both sides kept), after which GitHub answered
`MERGEABLE` and started five runs at the new head; the fingerprints `fc485f1927d1… → 799d32d6236e…`
are in a pull-request comment. The cost of not pairing the two reads was one full round of three.
