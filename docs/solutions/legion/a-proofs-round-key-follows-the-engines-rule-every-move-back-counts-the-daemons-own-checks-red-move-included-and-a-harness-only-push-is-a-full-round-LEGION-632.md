---
title: "A proof's round key follows the engine's rule — every move back counts, the daemon's own checks-red move included — and a harness-only push is a full round"
category: legion
tags:
  - stage-4b
  - review-round
  - handoff-fact
  - workflow-engine
  - e2e-harness
  - unchanged-diff-fingerprint
date: 2026-10-10
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh, scripts/e2e/lib/workflow.sh, packages/daemon/internal/workflow
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# A proof's round key follows the engine's rule — every move back counts, the daemon's own checks-red move included — and a harness-only push is a full round

Extends docs/solutions/legion/a-conflict-round-that-changes-the-fingerprint-is-a-counted-review-round-and-its-approval-starts-a-second-retro-LEGION-578.md.

- The implementer's `phases.rounds` counts every move to an earlier phase, not only a reviewer's
  `changes_requested`: `movesBack` (`packages/daemon/internal/workflow/engine.go`, `phaseIndex(to) <
  phaseIndex(from)`) calls `recordRound` for the tester's fail, a worker's backward move, and the
  move the daemon makes itself on a red CI verdict or a conflicting head (`TriggerChecksRed` from
  Testing, Reviewing and AwaitingMerge, `workflow/table.go`), since main's #1850. A proof that keys
  a handoff fact by round (`handoff_fact_commit ISSUE ROLE PHASE ROUND`, `scripts/e2e/lib/workflow.sh`:
  `review_round` must equal `ROUND`, else the fact is empty and the check fails as "no … round N
  handoff fact") must count the daemon's own moves in its expected round, not just the reviews the
  proof scripted.
- Say which "round" a number is. The proof's `round_line 1`, `round_correction_pushed 1` and
  `reviewer_decision 2` label the proof's own correction and count its reviews; the daemon's round
  after `ci-red-takeover` (one checks-red move back) is already 1 before the first review and 2
  after the review requests changes. Stage 4b's six readers of the daemon's round after the takeover
  were off by one on main and on every branch carrying it until bb12c138 (`tester testing 1`,
  `reviewer reviewing 2`, `assert_review_of_own_handoff … 2`, `assert_round_handoff … 2`,
  `implementer implementing 2`, `tester testing 2`); a comment at the first of them names the rule.
  When you add a move back to the engine, or a daemon-made move to a proof, walk every
  `assert_handoff_committer`, `assert_round_handoff` and `assert_review_of_own_handoff` after it.
- A proof reads the engine's live counter but types its expectation by hand: `review_round` queries
  the daemon's `phases` row, and nothing in the harness derives the expected number from the
  engine's rule. Until a helper counts the proof's own moves back, the hand-typed literals are the
  contract, and the engine's rule is the thing to re-read before each.
- A push that changes only the harness is a full round by the unchanged-diff fingerprint, which
  hashes the branch's whole diff against main with no carve-out for `scripts/e2e/`: bb12c138 (one
  file, six integers) cost a tester round and a second GitHub review at 9558a291, while the forward
  merge 3366e977 that changed nothing of the branch's own diff was a confirmation. Put a harness
  fix the current round's proof needs into that round; a harness fix nothing waits on goes to the
  fast-follow.

## Evidence

#1842's full stage 4b at 611e14df (2026-10-09 20:20–21:23Z, PR comment 6089500379) passed every
checkpoint through `ci-red-takeover` and failed `tree-reviewed` with `LEGSMOKE-591 has no tester
testing round 0 handoff fact`: the takeover's checks-red move had counted the implementer a round,
so `review_round` was 1 where the script asked for 0. The tester traced it to #1850 (`.legion/LEGION-632/test.json`
`observations`); the script was byte-identical to main's at those lines (main f3e990f7:2572 and
siblings), so main's own proof carried the defect; the architect directed one harness commit
(bb12c138, `scripts/e2e/stage4b-sandbox-tree.sh:2814-2888`), and the reviewer counted it a new
round by the fingerprint rule (`review.json` `round`). The coordinator reran stage 4b at the merged
head on that head's own image.
