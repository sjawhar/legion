---
title: "The first simplify pass lands the duplication it finds; a plan's self-contained choice is no reason to skip"
category: legion
tags:
  - simplify-pass
  - fast-follow
  - no-deferral
  - review-rounds
  - live-proof
date: 2026-10-26
status: active
module: legion-worker
related_issues:
  - "INFRA-272"
  - "sjawhar/legion#2223"
---

# The first simplify pass lands the duplication it finds; a plan's self-contained choice is no reason to skip

Extends docs/solutions/legion/a-fast-follow-line-that-promises-a-pull-request-after-the-merge-has-no-owner-once-the-tree-is-signed-off-land-the-cleanup-in-the-running-review-round-or-write-none-LEGION-695.md.

That document lands the cleanup in the review round that is running. Do it earlier, in the
implementer's first-round simplify pass, before any reviewer asks:

- **Apply a duplication finding in that pass.** This covers new code that copies an existing
  harness, script or helper. A plan that chose a "self-contained copy, so no existing proof
  changes" is no reason to skip it. Run the existing proof again on the shared code instead, which
  costs one more run.
- **Write nothing on `Fast-follow:` for this deployment.** The deployment's rule, and the human's
  ruling of 2026-10-26 (dispatch://INFRA-49/ask/2d080aec-ce9a-4c7c-a235-ae998d2b0830), forbid
  deferral, so the architect sends every Fast-follow back as another implementing round. A
  reviewer's Fast-follow goes into the open round too.
- **Treat the skill's allowance as superseded here.** `skills/legion-worker/SKILL.md` and
  `references/pr-body.md` still let naming, duplication or wording cleanup wait on the
  `Fast-follow:` line. On this deployment that line is `none`.

## Evidence

On sjawhar/legion#2223, the simplify pass found that the new `no-required-checks` driver and
script copied `merged-root-waits`. It skipped sharing them, citing the plan. The reviewer then
named the duplication as a Fast-follow, and the architect sent it back under the no-deferral
ruling. Round 2 shared the run (`scripts/e2e/lib/live-daemon-proof.sh`) and the Go harness. Rounds
3 and 4 each made one more harness Fast-follow the reviewer named. That reached the review-round
cap, and the architect narrowed. Three of the four implementing rounds went to work the first pass
had already found.
