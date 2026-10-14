---
title: "A rule that writes in a human's name takes its authority from rows the server wrote, and holds a client-editable tree to the server's own writer over its whole owned set"
category: architecture-patterns
tags:
  - authorization
  - server-owned-attributes
  - document-socket
  - approval
  - decision-blocks
  - settlement
  - review-rounds
date: 2026-10-14
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "LEGION-573"
  - "sjawhar/legion#1944"
  - "LEGION-752"
symptoms:
  - "a server rule reads who answered a block from the document tree's own attributes"
  - "a comparison hand-picks the attributes it checks and a review round names one more"
  - "a rule compares a version with the previous version where its premise is the version a human approved"
  - "the same hole is found in the same function on three consecutive review rounds"
---

# A rule that writes in a human's name takes its authority from rows the server wrote, and holds a client-editable tree to the server's own writer over its whole owned set

A Dispatch document's tree is written by every peer on its document socket: an attribute the
server owns on the markdown path (`pmdoc.StripServerOwnedAttrs` runs on markdown writes alone)
reaches the live tree verbatim from a socket peer, and a version named from that tree inside the
settle window stores it before settlement repairs it. A rule that records something in a human's
name on the strength of that tree is recording the peer's claim.

## The rule

- Who did what is the row the human's own authenticated route wrote, never the tree. Before
  writing the predicate, list every writer of each input (the HTTP routes *and* the CRDT socket
  path) and write the rule over the table only an authenticated-human route can write. In the
  carry (`docs/approval_carry.go`, `carryApproval`), a block's state, answerer and time are the
  document's `asks` rows (`loadAskBlocks`, `rowBackedBlocks`), and the candidates are the rows'
  answerers (`answerers`, `candidateApproval`).
- A tree node that must agree with a row is not compared field by field: run the server's own
  writer on a copy of the node with the row and require it to change nothing, over the writer's own
  named attribute set. `blockAsItsRowHasIt` runs `setAskServerAttributes` (`docs/ask_blocks.go`)
  over `askServerAttributeNames`; a node that differs on any of the eight refuses the carry. The
  part of a decided block the writer does not own, its wording, is held with settlement's own
  test (`askBlock.asks(model.NewAskEditPrevious(ask))`).
- A node no row of this document backs, a copy of another document's block, a node settlement
  could not read, and a block id the tree repeats each refuse the whole rule (`askNodes`,
  `rowBackedBlocks`). Refuse wherever the indexer you borrow authority from refuses: settlement
  indexes the first node of a repeated id, so "first node wins" is a door.
- A consent rule compares against what was consented to. The baseline is the version the human
  approved, `A`, alone; every read is `N`, `A`, or a row, and a third source (the previous version,
  the live tree) is a bug. Each block of `N` that is not as `A` held it must be the candidate's own
  answer, given after the approval being carried (`carryVerdict`); at least one must differ, and
  none of `A`'s may be gone.
- Enumerate the state machine of the thing you compare, not the sentence the spec wrote about it.
  "Every block added since his approval" is one transition of six (added, answered since by anyone,
  re-answered, resolved, removed, restored, reworded); each gets a row in the decision table and an
  acceptance line before the first round.
- An input that is repaired asynchronously (settlement rewrites a decided block's attributes and
  wording from its row) needs an in-window test from the first test pass
  (`DISPATCH_TEST_SETTLE_DELAY`, the `settlingCarryHandler` helpers), not after a reviewer names
  the window.
- Answer a review finding at the level of its class. "You trusted X" asks whether the server
  already has a component whose job is to make X true; route the check through it. Fixing the
  named instance leaves the class for the next round.

## Tests that pin it

- `TestRowBackedBlocksReadRowsAndRefuseAForgedACopiedAndAnInvalidNode`,
  `TestAnApprovedVersionsBlocksAreHeldAsTheirRows`,
  `TestAskNodesOfTheCarriedVersionReadEveryBlockOnceAndRefuseARepeatedID`,
  `TestCarryVerdictOverTheBlocksOfTheApprovedAndTheNewVersion` (pure, `docs/approval_carry_test.go`).
- `TestAForgedAnsweredBlockOnTheSocketCarriesNoApproval`,
  `TestAVersionHoldingARepeatedBlockIDCarriesNothing` and
  `TestAnApprovedVersionHoldingARepeatedBlockIDCarriesNothing` (through the live room, `docs`).
- `TestAForgedServerAttributeOnADecidedBlockCarriesNothing` (`api/approval_carry_test.go`: one
  subtest per name in `askServerAttributeNames`, through the live attribute path and
  `POST /versions` inside the settle window), `TestARewordOfAnAnsweredBlockInTheAnswersVersionDoesNotCarry`,
  `TestARestoredBlockAnsweredBeforeTheApprovalCarriesNothing`,
  `TestACopiedBlockPlantedSinceHisApprovalDoesNotCarry`, `TestTwoHumansAnswersInOneVersionDoNotCarry`.
- `TestAnApprovedVersionShowingADecisionOtherThanItsRowsCarriesNothing` and
  `TestAnApprovedVersionShowingADecidedBlockUnderOtherWordingCarriesNothing`
  (`api/approval_carry_approved_test.go`: the approved version held to the rows, the human's
  approval landing inside the settle window on a version settlement had not yet written the row
  back into; the `row-true` subtest is the control that carries).

## Evidence

sjawhar/legion#1944 (LEGION-573) took three `REQUEST_CHANGES` rounds on one function, each one
more instance of the same class:

1. Round 1 (Critical): the carry read `state`/`answered_by` from the version tree. A socket peer's
   node with those attributes and an unreadable body carried the approver's approval of a block he
   never answered. Fix: the rows are the authority.
2. Round 2 (Important): a decided block's wording was outside every comparison, so an agent's
   reword inside the settle window carried the approval of a choice under words nobody decided.
   Fix: settlement's own wording test. The same round removed the `N-1` read: comparing with the
   previous version counted a deleted-and-restored block as newly answered.
3. Round 3 (Important): the block-vs-row check compared three attributes while the browser renders
   the decision from `selected`/`answer`, and a repeated block id was read on its first node. Fix:
   settlement's own writer over the whole set (`blockAsItsRowHasIt`), the repeat refused.

The structural remedy of round 3 was visible in round 1's own finding. Proven at each round on a
Dispatch built from the branch against an embedded Postgres, over HTTP and the document socket
with the dashboard's own `yjs` + `@hocuspocus/provider`, with the previous round's binary as the
negative control.

Recorded at the merge and closed by LEGION-752: the approved version `A`'s nodes were read on the
three state attributes (`askBlockStates`, `shownAskState`), so a stored approved version whose
decided block carried a non-row `selected`, `answer`, copy provenance or question compared as
unchanged against a row-true `N`. `A`'s blocks are now held to `N`'s rows by the same predicate
as `N`'s own (`blockAsSettlementLeavesIt`, through `heldAsItsRow`), the three-attribute reading
is gone, and `N`'s blocks travel as their rows; `A`'s nodes are read through `askNodes` as `N`'s
are, though a stored version's read-back (`pmdoc.ParseRendering`) repairs a repeated block id
before the carry reads it, so a stored repeat never carried on either side - it shows `A` holding
a block `N` never had. Pinned by `TestAnApprovedVersionShowingADecisionOtherThanItsRowsCarriesNothing`,
`TestAnApprovedVersionShowingADecidedBlockUnderOtherWordingCarriesNothing` and
`TestAnApprovedVersionHoldingARepeatedBlockIDCarriesNothing`.
