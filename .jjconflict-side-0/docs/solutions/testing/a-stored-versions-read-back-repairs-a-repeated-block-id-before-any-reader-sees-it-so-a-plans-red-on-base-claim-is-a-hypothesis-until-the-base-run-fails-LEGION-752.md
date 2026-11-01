---
title: "A stored version's read-back repairs a repeated block id before any reader sees it, so a plan's red-on-base claim is a hypothesis until the base run fails"
category: testing
tags:
  - red-on-base
  - regression-tests
  - pmdoc
  - block-ids
  - approval-carry
  - plan-verification
date: 2026-10-14
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "LEGION-752"
  - "sjawhar/legion#1957"
---

# A stored version's read-back repairs a repeated block id before any reader sees it, so a plan's red-on-base claim is a hypothesis until the base run fails

- A plan's sentence "this test is red today because X" is a prediction about the base, not a
  fact about it. Run the new test on the base tree first - before the rest of the test commit,
  and before any doc comment, changelog line or PR body narrates why it is red
  (`docs/solutions/testing/red-on-main-for-a-go-daemon-test-is-a-git-archive-of-main-from-the-jj-git-store-plus-the-branchs-test-files-LEGION-698.md`
  for the base tree). A test that passes on the base is still worth keeping when it pins
  behaviour nobody had pinned, but it is a pin, and every text that calls it red is wrong.
- Read the layer between the stored bytes and the code under test before predicting what that
  code sees. Dispatch reads a stored document version back through `pmdoc.ParseRendering`, and
  `parseStampedAt` (`pmdoc/parse.go`) runs `EnsureBlockIDs` on every parse: a block id the
  markdown holds twice keeps the id on its first block and the next block gets a freshly minted
  one (`EnsureBlockIDsCount`, `pmdoc/block_ids.go`). No reader of a stored version ever sees a
  repeated block id. A rule that must treat a stored repeat specially reads the markdown before
  the parse, or `BlockIDRepairCount`; a rule that only needs the repeat to refuse something may
  already get that for free from the minted id (an extra block the other side never had).
- When a plan's premise fails this way, say so where the next reader looks - the handoff's
  `deviations`, the PR body, the doc comment of the branch the plan asked for - and name the
  mechanism that does the work instead. A branch kept for the symmetry of a rule, with an honest
  comment that it is unreachable from this caller and why, is a decision for the architect to
  confirm, not one to make silently in either direction.

## Evidence (LEGION-752, sjawhar/legion#1957)

The plan said `TestAnApprovedVersionHoldingARepeatedBlockIDCarriesNothing` would fail on the base
because the approved version's reader (`askBlockStates`) kept the first node of a repeated id and
so read alice's later answer as her own change. The deep-worker writing the test ran it on the
exported base tree before reporting and found it green: a scratch probe showed `askNodes` on the
parsed stored version returning `readable=true` with ids `[region <minted uuid>]` - the parse had
already repaired the repeat, so the approved version showed a block the new version never had,
and `carryVerdict`'s "every block of A still stands" test refused on the base as it does at the
head. The two api tests of the same plan (`TestAnApprovedVersionShowingADecisionOtherThanItsRowsCarriesNothing`,
`TestAnApprovedVersionShowingADecidedBlockUnderOtherWordingCarriesNothing`) were red on the base
as predicted. The `askNodes` read in `readApprovedVersion` stayed, with its doc comment saying the
refusal is unreachable from a stored version and what refuses a stored repeat instead; the
architect confirmed keeping it (A and N read through one predicate; the parser's repair lives in
another package the carry should not depend on).
