---
title: "A settlement reuses an earlier result only for the same tree, with an unshrinking record that nothing changed it"
category: envoy
tags:
  - dispatch
  - settlement
  - caching
  - reuse-invariant
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
---

# A settlement reuses an earlier result only for the same tree, with an unshrinking record that nothing changed it

- Before `settleRoomWithin` (or any other path) reuses a result it computed from the tree it read
  (a render, offsets, a token), require two things. The tree it would compute again must be the
  same object (`versioned == read`). And a record that only ever grows must show that nothing
  changed that object in place.
- "Wrote nothing into the room" (no suppression slot) does not mean "the tree is unchanged". A
  write that changes nothing in the live document returns no slot, yet the settlement may already
  have repaired its own copy in memory, or replaced it with a stamp's tree.
- Never build the "untouched" check from a list a later step prunes. `withholdAnswers` restores a
  block and drops its repair from `repairs`, so `repairs` alone can end empty after the tree was
  touched. Count the list nothing shrinks as well (`rewrites`).
- Write the invariant at the reuse site and in `packages/envoy/AGENTS.md`'s settlement paragraph.
  Name where the read comes from (`readSettlementTree`, `liveTree`) and its one in-place writer
  (`reconcileAskBlocks`, recorded by `changedTree`), as the comment above the reuse decision in
  `settleRoomWithin` and that paragraph do, so a later change to the read path keeps it. A new
  in-place writer that is not recorded silently versions a stale tree. The settlement tests catch
  one only on the settlements they drive: `requireReadUnchangedWhenReused`
  (`settlement_read_invariant_test.go`) fails a test whose settlement reuses the rendering of a read
  that changed after `readSettlementTree` returned it, and the tests in
  `settlement_read_invariant_test.go` and `settlement_render_once_test.go` install it. A writer on a
  path no test drives is caught only in review.
- Treat a reuse condition written in a parent issue's ranking or design note as a hypothesis.
  Check it against the code, and use the wrong condition as the negative control of your tests.

## Evidence

- The reuse is at `service.go:1599-1602`, inside `if versioned != read || reconciliation.changedTree()`.
  `changedTree` (`ask_blocks.go:114-121`) is `len(r.repairs) > 0 || len(r.rewrites) > 0`.
- `applySuppressed` returns no slot when its write changed nothing (`service.go:425-427`). A
  stamp that mints nothing still sets `reconciled` to the stamp's own tree (`service.go:1462-1469`).
- `withholdAnswers` restores the block (`found.restore`) and drops the repair
  (`ask_answers.go:86,101-111`). `rewrites` is never pruned.
- The fix ranking's condition (`len(slots) == 0`) would have versioned the unrepaired or
  unstamped read. Under it, 42 of 126 versions in an equivalence run on 21 real specs differed
  from the room: every "repair the room already holds" case and every "stamp that minted
  nothing" case (PR #2416 body, `E2E (implementer)`). Removing either half of the correct
  condition fails one of the two regression tests: with `versioned != read` alone the repair test
  fails, and with `reconciliation.changedTree()` alone the stamp test fails.
